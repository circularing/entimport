package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/circularing/entimport/internal/entimport"
	"github.com/circularing/entimport/internal/mux"
)

var (
	tablesFlag        tables
	excludeTablesFlag tables
	ignoreMissingPK   bool
	camelCaseFlag     bool
)

func init() {
	flag.Var(&tablesFlag, "tables", "comma-separated list of tables to inspect (all if empty)")
	flag.Var(&excludeTablesFlag, "exclude-tables", "comma-separated list of tables to exclude")
	flag.BoolVar(&ignoreMissingPK, "ignore-missing-pk", false, "ignore missing primary keys and use foreign keys as fallback")
	flag.BoolVar(&camelCaseFlag, "camelcase", false, "convert snake_case table and column names to camelCase in generated schema")
}

func main() {
	dsn := flag.String("dsn", "",
		`data source name (connection information), for example:
"mysql://user:pass@tcp(localhost:3306)/dbname"
"postgres://user:pass@host:port/dbname"`)
	schemaPath := flag.String("schema-path", "./ent/schema", "output path for ent schema")
	flag.Parse()
	if *dsn == "" {
		log.Println("entimport: data source name (dsn) must be provided")
		flag.Usage()
		os.Exit(2)
	}
	ctx := context.Background()
	drv, err := mux.Default.OpenImport(*dsn)
	if err != nil {
		log.Fatalf("entimport: failed to create import driver - %v", err)
	}
	i, err := entimport.NewImport(
		entimport.WithTables(tablesFlag),
		entimport.WithExcludedTables(excludeTablesFlag),
		entimport.WithDriver(drv),
		entimport.WithIgnoreMissingPrimaryKey(ignoreMissingPK),
		entimport.WithCamelCase(camelCaseFlag),
	)
	if err != nil {
		log.Fatalf("entimport: create importer failed: %v", err)
	}
	mutations, err := i.SchemaMutations(ctx)
	if err != nil {
		log.Fatalf("entimport: schema import failed - %v", err)
	}
	if err = entimport.WriteSchema(mutations, entimport.WithSchemaPath(*schemaPath)); err != nil {
		log.Fatalf("entimport: schema writing failed - %v", err)
	}
}

type tables []string

func (t *tables) String() string {
	return fmt.Sprint(*t)
}

func (t *tables) Set(s string) error {
	*t = strings.Split(s, ",")
	return nil
}
