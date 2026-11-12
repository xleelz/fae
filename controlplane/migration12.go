package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Migration12 preserves human-readable recursive IDs byte-for-byte. Dolt
// supports these composite binary keys (including both backlog edge endpoints);
// no digest replaces a task's externally visible identity.
var Migration12 = []string{`CREATE TABLE IF NOT EXISTS task_tree (task_id VARBINARY(2048) PRIMARY KEY,tree_root VARBINARY(2048) NOT NULL,parent_task VARBINARY(2048) NOT NULL,depth BIGINT NOT NULL,detached BOOLEAN NOT NULL,INDEX tree_members(tree_root,task_id))`}

const MaxTaskIDBytes = 2048

var createTableName = regexp.MustCompile(`(?i)CREATE TABLE(?: IF NOT EXISTS)? ([a-z_]+)\s*\(`)
var treeIdentityColumn = regexp.MustCompile(`(?i)(?:\(|,)\s*(task_id|job_id|item_id|from_id|to_id|dispatch_id|session_id)\s+VARCHAR\(`)

func migration12Columns(ctx context.Context, tx *sql.Tx) error {
	statements := [][]string{SchemaStatements, Migration2, Migration3, Migration4, Migration5, Migration6, Migration7, Migration8, Migration9, Migration10, Migration11, {TurnCheckpointsTableDDL}}
	pairs := map[string]map[string]bool{}
	for _, group := range statements {
		for _, ddl := range group {
			table := createTableName.FindStringSubmatch(ddl)
			if len(table) == 0 {
				continue
			}
			for _, m := range treeIdentityColumn.FindAllStringSubmatch(ddl, -1) {
				if pairs[table[1]] == nil {
					pairs[table[1]] = map[string]bool{}
				}
				pairs[table[1]][m[1]] = true
			}
		}
	}
	// Migration9 added these references with ALTER rather than CREATE.
	for _, p := range [][2]string{{"external_effects", "session_id"}, {"engine_runs", "dispatch_id"}} {
		if pairs[p[0]] == nil {
			pairs[p[0]] = map[string]bool{}
		}
		pairs[p[0]][p[1]] = true
	}
	tables := []string{}
	for table := range pairs {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		columns := []string{}
		for column := range pairs[table] {
			columns = append(columns, column)
		}
		sort.Strings(columns)
		for _, column := range columns {
			var typ, nullable string
			var length sql.NullInt64
			var defaultValue sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH,IS_NULLABLE,COLUMN_DEFAULT FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name=? AND column_name=?`, table, column).Scan(&typ, &length, &nullable, &defaultValue)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return err
			}
			size := MaxTaskIDBytes
			if column == "dispatch_id" || column == "session_id" {
				size = 3072
			}
			if strings.EqualFold(typ, "varbinary") && length.Int64 >= int64(size) {
				continue
			}
			ddl := fmt.Sprintf("ALTER TABLE `%s` MODIFY COLUMN `%s` VARBINARY(%d)", table, column, size)
			if nullable == "YES" {
				ddl += " NULL"
			} else {
				ddl += " NOT NULL"
			}
			if defaultValue.Valid {
				ddl += " DEFAULT '" + strings.ReplaceAll(defaultValue.String, "'", "''") + "'"
			}
			if _, err = tx.ExecContext(ctx, ddl); err != nil {
				return err
			}
		}
	}
	return nil
}
