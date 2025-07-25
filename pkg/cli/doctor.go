// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package cli

import (
	"bufio"
	"context"
	"database/sql/driver"
	hx "encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	apd "github.com/cockroachdb/apd/v3"
	"github.com/cockroachdb/cockroach/pkg/cli/clierror"
	"github.com/cockroachdb/cockroach/pkg/cli/clierrorplus"
	"github.com/cockroachdb/cockroach/pkg/cli/clisqlclient"
	"github.com/cockroachdb/cockroach/pkg/cli/exit"
	"github.com/cockroachdb/cockroach/pkg/clusterversion"
	"github.com/cockroachdb/cockroach/pkg/jobs"
	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/doctor"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/builtins"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/protoutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/spf13/cobra"
)

var debugDoctorCmd = &cobra.Command{
	Use:   "doctor [command]",
	Short: "run a cockroach doctor tool command",
	Long: `
Run the doctor tool to recreate or examine cockroach system table contents.
System tables are queried either from a live cluster or from an unzipped
debug.zip.
`,
}

var doctorExamineCmd = &cobra.Command{
	Use:   "examine [cluster|zipdir]",
	Short: "examine system tables for inconsistencies",
	Long: `
Run the doctor tool to examine the system table contents and perform validation
checks. System tables are queried either from a live cluster or from an unzipped
debug.zip.
`,
}

var doctorRecreateCmd = &cobra.Command{
	Use:   "recreate [cluster|zipdir]",
	Short: "prints SQL that tries to recreate system table state",
	Long: `
Run the doctor tool to examine system tables and generate SQL statements that,
when run on an empty cluster, recreate that state as closely as possible. System
tables are queried either from a live cluster or from an unzipped debug.zip.
`,
}

type doctorFn = func(
	version *clusterversion.ClusterVersion,
	descTable doctor.DescriptorTable,
	namespaceTable doctor.NamespaceTable,
	jobsTable doctor.JobsTable,
	out io.Writer,
) (err error)

func makeZipDirCommand(fn doctorFn) *cobra.Command {
	return &cobra.Command{
		Use:   "zipdir <debug_zip_dir> [version]",
		Short: "run doctor tool on data from an unzipped debug.zip",
		Long: `
Run the doctor tool on system data from an unzipped debug.zip. This command
requires the path of the unzipped 'debug' directory as its argument. A version
can be optionally specified, which will be used enable / disable validation
that may not exist on downlevel versions.
`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			descs, ns, jobs, err := fromZipDir(args[0])
			var version *clusterversion.ClusterVersion
			if len(args) == 2 {
				version = &clusterversion.ClusterVersion{
					Version: roachpb.MustParseVersion(args[1]),
				}
			}
			if err != nil {
				return err
			}
			return fn(version, descs, ns, jobs, os.Stdout)
		},
	}
}

func makeClusterCommand(fn doctorFn) *cobra.Command {
	return &cobra.Command{
		Use:   "cluster --url=<cluster connection string>",
		Short: "run doctor tool on live cockroach cluster",
		Long: `
Run the doctor tool system data from a live cluster specified by --url.
`,
		Args: cobra.NoArgs,
		RunE: clierrorplus.MaybeDecorateError(
			func(cmd *cobra.Command, args []string) (resErr error) {
				sqlConn, err := makeSQLClient(context.Background(), "cockroach doctor", useSystemDb)
				if err != nil {
					return errors.Wrap(err, "could not establish connection to cluster")
				}
				defer func() { resErr = errors.CombineErrors(resErr, sqlConn.Close()) }()
				descs, ns, jobs, err := fromCluster(sqlConn, cliCtx.cmdTimeout)
				if err != nil {
					return err
				}
				return fn(nil, descs, ns, jobs, os.Stdout)
			}),
	}
}

func deprecateCommand(cmd *cobra.Command) *cobra.Command {
	cmd.Hidden = true
	cmd.Deprecated = fmt.Sprintf("use 'doctor examine %s' instead.", cmd.Name())
	return cmd
}

var doctorExamineClusterCmd = makeClusterCommand(runDoctorExamine)
var doctorExamineZipDirCmd = makeZipDirCommand(runDoctorExamine)
var doctorExamineFallbackClusterCmd = deprecateCommand(makeClusterCommand(runDoctorExamine))
var doctorExamineFallbackZipDirCmd = deprecateCommand(makeZipDirCommand(runDoctorExamine))
var doctorRecreateClusterCmd = makeClusterCommand(runDoctorRecreate)
var doctorRecreateZipDirCmd = makeZipDirCommand(runDoctorRecreate)

func runDoctorRecreate(
	_ *clusterversion.ClusterVersion,
	descTable doctor.DescriptorTable,
	namespaceTable doctor.NamespaceTable,
	jobsTable doctor.JobsTable,
	out io.Writer,
) (err error) {
	return doctor.DumpSQL(out, descTable, namespaceTable)
}

func runDoctorExamine(
	version *clusterversion.ClusterVersion,
	descTable doctor.DescriptorTable,
	namespaceTable doctor.NamespaceTable,
	jobsTable doctor.JobsTable,
	out io.Writer,
) (err error) {
	if version == nil {
		version = &clusterversion.ClusterVersion{
			Version: clusterversion.DoctorBinaryVersion,
		}
	}
	var valid bool
	valid, err = doctor.Examine(
		context.Background(),
		*version,
		descTable,
		namespaceTable,
		jobsTable,
		true, /*validateJobs*/
		debugCtx.verbose,
		out)
	if err != nil {
		return err
	}
	if !valid {
		return clierror.NewError(errors.New("validation failed"),
			exit.DoctorValidationFailed())
	}
	fmt.Fprintln(out, "No problems found!")
	return nil
}

// fromCluster collects system table data from a live cluster.
func fromCluster(
	sqlConn clisqlclient.Conn, timeout time.Duration,
) (
	descTable doctor.DescriptorTable,
	namespaceTable doctor.NamespaceTable,
	jobsTable doctor.JobsTable,
	retErr error,
) {
	ctx := context.Background()
	if err := sqlConn.EnsureConn(ctx); err != nil {
		return nil, nil, nil, err
	}

	if !buildutil.CrdbTestBuild {
		if err := sqlConn.Exec(ctx, "BEGIN AS OF SYSTEM TIME '-0.1s';"); err != nil {
			return nil, nil, nil, err
		}
		defer func() {
			if err := sqlConn.Exec(ctx, "ROLLBACK;"); err != nil {
				retErr = errors.WithSecondaryError(retErr, errors.Wrapf(err, "failed rolling back"))
			}
		}()
	}

	if timeout != 0 {
		if err := sqlConn.Exec(ctx,
			`SET statement_timeout = $1`, timeout.String()); err != nil {
			return nil, nil, nil, err
		}
	}
	stmt := `
SELECT id, descriptor, crdb_internal_mvcc_timestamp AS mod_time_logical
FROM system.descriptor ORDER BY id`
	checkColumnExistsStmt := "SELECT crdb_internal_mvcc_timestamp FROM system.descriptor LIMIT 1"
	_, err := sqlConn.QueryRow(ctx, checkColumnExistsStmt)
	// On versions before 20.2, the system.descriptor won't have the builtin
	// crdb_internal_mvcc_timestamp. If we can't find it, use NULL instead.
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) {
		if pgcode.MakeCode(pgErr.Code) == pgcode.UndefinedColumn {
			stmt = `
SELECT id, descriptor, NULL AS mod_time_logical
FROM system.descriptor ORDER BY id`
		}
	} else if err != nil {
		return nil, nil, nil, err
	}
	descTable = make([]doctor.DescriptorTableRow, 0)

	if err := selectRowsMap(sqlConn, stmt, make([]driver.Value, 3), func(vals []driver.Value) error {
		var row doctor.DescriptorTableRow
		if id, ok := vals[0].(int64); ok {
			row.ID = id
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[0], vals[0])
		}
		if descBytes, ok := vals[1].([]byte); ok {
			row.DescBytes = descBytes
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[1], vals[1])
		}
		if vals[2] == nil {
			row.ModTime = hlc.Timestamp{WallTime: timeutil.Now().UnixNano()}
		} else if mt, ok := vals[2].(pgtype.Numeric); ok {
			buf, err := mt.MarshalJSON()
			if err != nil {
				return err
			}
			decimal, _, err := apd.NewFromString(string(buf))
			if err != nil {
				return err
			}
			ts, err := hlc.DecimalToHLC(decimal)
			if err != nil {
				return err
			}
			row.ModTime = ts
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[2], vals[2])
		}
		descTable = append(descTable, row)
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	stmt = `SELECT "parentID", "parentSchemaID", name, id FROM system.namespace`

	checkColumnExistsStmt = `SELECT "parentSchemaID" FROM system.namespace LIMIT 1`
	_, err = sqlConn.QueryRow(ctx, checkColumnExistsStmt)
	// On versions before 20.1, table system.namespace does not have this column.
	// In that case the ParentSchemaID for tables is 29 and for databases is 0.
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) {
		if pgcode.MakeCode(pgErr.Code) == pgcode.UndefinedColumn {
			stmt = `
SELECT "parentID", CASE WHEN "parentID" = 0 THEN 0 ELSE 29 END AS "parentSchemaID", name, id
FROM system.namespace`
		}
	} else if err != nil {
		return nil, nil, nil, err
	}

	namespaceTable = make([]doctor.NamespaceTableRow, 0)
	if err := selectRowsMap(sqlConn, stmt, make([]driver.Value, 4), func(vals []driver.Value) error {
		var row doctor.NamespaceTableRow
		if parentID, ok := vals[0].(int64); ok {
			row.ParentID = descpb.ID(parentID)
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[0], vals[0])
		}
		if schemaID, ok := vals[1].(int64); ok {
			row.ParentSchemaID = descpb.ID(schemaID)
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[0], vals[0])
		}
		if name, ok := vals[2].(string); ok {
			row.Name = name
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[1], vals[1])
		}
		if id, ok := vals[3].(int64); ok {
			row.ID = id
		} else {
			return errors.Errorf("unexpected value: %T of %v", vals[0], vals[0])
		}
		namespaceTable = append(namespaceTable, row)
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	stmt = `
SELECT id, status, payload, progress FROM "".crdb_internal.system_jobs
`
	checkSystemJobs := `SELECT count(*) FROM "".crdb_internal.system_jobs LIMIT 1`
	_, err = sqlConn.QueryRow(ctx, checkSystemJobs)
	// On versions after 23.1, we have a new table called crdb_internal.system_jobs;
	// if its not available fall back to system.jobs
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) {
		if pgcode.MakeCode(pgErr.Code) == pgcode.UndefinedTable {
			stmt = `
SELECT id, status, payload, progress FROM system.jobs
`
		}
	} else if err != nil {
		return nil, nil, nil, err
	}
	jobsTable = make(doctor.JobsTable, 0)

	if err := selectRowsMap(sqlConn, stmt, make([]driver.Value, 4), func(vals []driver.Value) error {
		md := jobs.JobMetadata{}
		md.ID = jobspb.JobID(vals[0].(int64))
		md.State = jobs.State(vals[1].(string))
		md.Payload = &jobspb.Payload{}
		if err := protoutil.Unmarshal(vals[2].([]byte), md.Payload); err != nil {
			return err
		}
		md.Progress = &jobspb.Progress{}
		// Progress is a nullable column, so have to check for nil here.
		if progressBytes, ok := vals[3].([]byte); ok {
			if err := protoutil.Unmarshal(progressBytes, md.Progress); err != nil {
				return err
			}
		}
		jobsTable = append(jobsTable, md)
		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	return descTable, namespaceTable, jobsTable, nil
}

// fromZipDir collects system table data from a decompressed debug zip dir.
func fromZipDir(
	zipDirPath string,
) (
	descTable doctor.DescriptorTable,
	namespaceTable doctor.NamespaceTable,
	jobsTable doctor.JobsTable,
	retErr error,
) {
	// To make parsing user functions code happy.
	_ = builtins.AllBuiltinNames()

	descTable = make(doctor.DescriptorTable, 0)
	if checkIfFileExists(zipDirPath, "system.descriptor.txt") {
		if err := slurp(zipDirPath, "system.descriptor.txt", func(row string) error {
			fields := strings.Fields(row)
			if len(fields) != 2 {
				return errors.Errorf("expected 2 fields, got %d in system.descriptors.txt", len(fields))
			}
			last := len(fields) - 1
			i, err := strconv.Atoi(fields[0])
			if err != nil {
				return errors.Wrapf(err, "failed to parse descriptor id %s", fields[0])
			}

			descBytes, ok := interpretString(fields[last])
			if !ok {
				return errors.Newf("failed to decode hex descriptor %d", i)
			}
			ts := hlc.Timestamp{WallTime: timeutil.Now().UnixNano()}
			descTable = append(descTable, doctor.DescriptorTableRow{ID: int64(i), DescBytes: descBytes, ModTime: ts})
			return nil
		}); err != nil {
			return nil, nil, nil, err
		}
	} else {
		descTableJSON := make([]struct {
			ID         string `json:"id"`
			Descriptor string `json:"hex_descriptor"`
		}, 0)
		if err := parseJSONFile(zipDirPath, "system.descriptor.json", &descTableJSON); err != nil {
			return nil, nil, nil, errors.Wrapf(err, "failed to parse system.descriptor.json")
		}
		ts := hlc.Timestamp{WallTime: timeutil.Now().UnixNano()}
		for _, desc := range descTableJSON {
			var err error
			row := doctor.DescriptorTableRow{
				ModTime: ts,
			}
			row.ID, err = strconv.ParseInt(desc.ID, 10, 64)
			if err != nil {
				return nil, nil, nil, errors.Wrapf(err, "failed to decode hex descriptor: (%s)", desc.ID)
			}

			row.DescBytes = make([]byte, hx.DecodedLen(len(desc.Descriptor)))
			if _, err := hx.Decode(row.DescBytes, []byte(desc.Descriptor)); err != nil {
				return nil, nil, nil, errors.Wrapf(err, "failed to decode hex descriptor: (%s)", desc.Descriptor)
			}
			descTable = append(descTable, row)
		}
	}

	// Handle old debug zips where the namespace table dump is from namespace2.
	namespaceFileName := "system.namespace2.txt"
	if _, err := os.Stat(namespaceFileName); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			// Handle unexpected errors.
			return nil, nil, nil, err
		}
		namespaceFileName = "system.namespace.txt"
	}
	namespaceTable = make(doctor.NamespaceTable, 0)
	if checkIfFileExists(zipDirPath, namespaceFileName) {
		if err := slurp(zipDirPath, namespaceFileName, func(row string) error {
			fields := strings.Fields(row)
			if len(fields) != 4 {
				return errors.Errorf("expected 4 fields, got %d in system.namespace.txtq", len(fields))
			}
			parID, err := strconv.Atoi(fields[0])
			if err != nil {
				return errors.Wrapf(err, "failed to parse parent id %s", fields[0])
			}
			parSchemaID, err := strconv.Atoi(fields[1])
			if err != nil {
				return errors.Wrapf(err, "failed to parse parent schema id %s", fields[1])
			}
			id, err := strconv.Atoi(fields[3])
			if err != nil {
				if fields[3] == "NULL" {
					id = int(descpb.InvalidID)
				} else {
					return errors.Wrapf(err, "failed to parse id %s", fields[3])
				}
			}
			// Attempt to unquote any strings, if they aren't quoted,
			// we will use the original raw string.
			unquotedName := fields[2]
			if updatedName := strings.TrimSuffix(strings.TrimPrefix(unquotedName, "\""), "\""); updatedName != unquotedName {
				unquotedName = strings.Replace(updatedName, "\"\"", "\"", -1)
			}
			namespaceTable = append(namespaceTable, doctor.NamespaceTableRow{
				NameInfo: descpb.NameInfo{
					ParentID: descpb.ID(parID), ParentSchemaID: descpb.ID(parSchemaID), Name: unquotedName,
				},
				ID: int64(id),
			})
			return nil
		}); err != nil {
			return nil, nil, nil, err
		}
	} else {
		namespaceTableJSON := make([]struct {
			ID             string `json:"id"`
			ParentID       string `json:"parentID"`
			ParentSchemaID string `json:"parentSchemaID"`
			Name           string `json:"name"`
		}, 0)
		if err := parseJSONFile(zipDirPath, "system.namespace.json", &namespaceTableJSON); err != nil {
			return nil, nil, nil, errors.Wrapf(err, "failed to parse system.descriptor.json")
		}
		for _, namespace := range namespaceTableJSON {
			row := doctor.NamespaceTableRow{
				NameInfo: descpb.NameInfo{
					Name: namespace.Name,
				},
			}

			id, err := strconv.ParseInt(namespace.ParentID, 10, 64)
			if len(namespace.ParentID) > 0 && err != nil {
				return nil, nil, nil, errors.Wrapf(err, "failed to decode namespace ParentID %v", namespace)
			}
			row.ParentID = descpb.ID(id)
			id, err = strconv.ParseInt(namespace.ParentSchemaID, 10, 64)
			if len(namespace.ParentSchemaID) > 0 && err != nil {
				return nil, nil, nil, errors.Wrapf(err, "failed to decode namespace ParentSchemaID %v", namespace)
			}
			row.ParentSchemaID = descpb.ID(id)
			id, err = strconv.ParseInt(namespace.ID, 10, 64)
			if len(namespace.ID) > 0 && err != nil {
				return nil, nil, nil, errors.Wrapf(err, "failed to decode namespace ID %v", namespace)
			}
			row.ID = id
			namespaceTable = append(namespaceTable,
				row)
		}
	}

	jobsTable = make(doctor.JobsTable, 0)
	if checkIfFileExists(zipDirPath, "crdb_internal.system_jobs.txt") {
		if err := slurp(zipDirPath, "crdb_internal.system_jobs.txt", func(row string) error {
			fields := strings.Fields(row)
			if len(fields) < 6 {
				return errors.Errorf("expected at least 6 fields, got %d in crdb_internal.system_jobs.txt", len(fields))
			}
			md := jobs.JobMetadata{}
			md.State = jobs.State(fields[1])

			id, err := strconv.Atoi(fields[0])
			if err != nil {
				return errors.Wrapf(err, "failed to parse job id %s", fields[0])
			}
			md.ID = jobspb.JobID(id)

			// N.B. The "created" column takes 2 positions in our fields array.
			payloadBytes, ok := interpretString(fields[4])
			if !ok {
				return errors.Newf("job %d: failed to decode hex payload", id)
			}
			md.Payload = &jobspb.Payload{}
			if err := protoutil.Unmarshal(payloadBytes, md.Payload); err != nil {
				return errors.Wrap(err, "failed unmarshalling job payload")
			}

			// Skip NULL job payloads.
			if fields[5] == "NULL" {
				return nil
			}
			progressBytes, ok := interpretString(fields[5])
			if !ok {
				return errors.Newf("job %d: failed to decode hex progress", id)
			}
			md.Progress = &jobspb.Progress{}
			if err := protoutil.Unmarshal(progressBytes, md.Progress); err != nil {
				return errors.Wrap(err, "failed unmarshalling job progress")
			}

			jobsTable = append(jobsTable, md)
			return nil
		}); err != nil {
			return nil, nil, nil, err
		}
	} else {
		jobsTableJSON := make([]struct {
			ID       string `json:"id"`
			Status   string `json:"status"`
			Payload  string `json:"hex_payload"`
			Progress string `json:"hex_progress"`
		}, 0)
		if err := parseJSONFile(zipDirPath, "crdb_internal.system_jobs.json", &jobsTableJSON); err != nil {
			return nil, nil, nil, errors.Wrapf(err, "failed to parse crdb_internal.system_jobs.json")
		}
		for _, job := range jobsTableJSON {
			row := jobs.JobMetadata{
				State: jobs.State(job.Status),
			}
			id, err := strconv.ParseInt(job.ID, 10, 64)
			if len(job.ID) > 0 && err != nil {
				return nil, nil, nil, errors.Wrapf(err, "failed to decode job ID %v", job)
			}
			row.ID = jobspb.JobID(id)
			// Skip NULL job payloads.
			if job.Payload == "NULL" {
				continue
			}
			payloadBytes, err := hx.DecodeString(job.Payload)
			if err != nil {
				return nil, nil, nil, errors.Wrapf(err, "job %d: failed to decode hex payload", id)
			}
			row.Payload = &jobspb.Payload{}
			if err := protoutil.Unmarshal(payloadBytes, row.Payload); err != nil {
				return nil, nil, nil, errors.Wrap(err, "failed unmarshalling job payload")
			}

			progressBytes, err := hx.DecodeString(job.Progress)
			if err != nil && job.Progress != "NULL" {
				return nil, nil, nil, errors.Wrapf(err, "job %d: failed to decode hex progress", id)
			}
			row.Progress = &jobspb.Progress{}
			if err := protoutil.Unmarshal(progressBytes, row.Progress); err != nil {
				return nil, nil, nil, errors.Wrap(err, "failed unmarshalling job progress")
			}
			jobsTable = append(jobsTable,
				row)
		}
	}
	return descTable, namespaceTable, jobsTable, nil
}

func checkIfFileExists(zipDirPath string, fileName string) bool {
	_, err := os.Stat(path.Join(zipDirPath, fileName))
	return err == nil
}

// readJsonArray reads the given file as an array of JSON structs.
func parseJSONFile(zipDirPath string, fileName string, result interface{}) error {
	filePath := path.Join(zipDirPath, fileName)
	// Check for existence of companion .err.txt file.
	_, err := os.Stat(filePath + ".err.txt")
	if err == nil {
		// A .err.txt file exists.
		fmt.Printf("WARNING: errors occurred during the production of %s, contents may be missing or incomplete.\n", fileName)
	} else if !errors.Is(err, os.ErrNotExist) {
		// Handle unexpected errors.
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	return decoder.Decode(result)
}

// slurp reads a file in zipDirPath and processes its contents.
func slurp(zipDirPath string, fileName string, tableMapFn func(row string) error) error {
	filePath := path.Join(zipDirPath, fileName)

	// Check for existence of companion .err.txt file.
	_, err := os.Stat(filePath + ".err.txt")
	if err == nil {
		// A .err.txt file exists.
		fmt.Printf("WARNING: errors occurred during the production of %s, contents may be missing or incomplete.\n", fileName)
	} else if !errors.Is(err, os.ErrNotExist) {
		// Handle unexpected errors.
		return err
	}

	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	if debugCtx.verbose {
		fmt.Println("reading " + filePath)
	}
	return tableMap(f, tableMapFn)
}

// tableMap applies `fn` to all rows in `in`.
func tableMap(in io.Reader, fn func(string) error) error {
	firstLine := true
	sc := bufio.NewScanner(in)
	// Read lines up to 200 MB in size.
	sc.Buffer(make([]byte, 64*1024), 200*1024*1024)
	for sc.Scan() {
		if firstLine {
			firstLine = false
			continue
		}
		if err := fn(sc.Text()); err != nil {
			return err
		}
	}
	return sc.Err()
}

// selectRowsMap applies `fn` to all rows returned from a select statement.
func selectRowsMap(
	conn clisqlclient.Conn, stmt string, vals []driver.Value, fn func([]driver.Value) error,
) error {
	rows, err := conn.Query(context.Background(), stmt)
	if err != nil {
		return errors.Wrapf(err, "query '%s'", stmt)
	}
	for {
		var err error
		if err = rows.Next(vals); err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := fn(vals); err != nil {
			return err
		}
	}
	return nil
}
