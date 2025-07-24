package entimport

import (
	"context"
	"encoding/json"
	"fmt"

	"ariga.io/atlas/sql/postgres"
	"ariga.io/atlas/sql/schema"

	"entgo.io/contrib/schemast"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// Postgres implements SchemaImporter for PostgreSQL databases.
type Postgres struct {
	*ImportOptions
}

// NewPostgreSQL - returns a new *Postgres.
func NewPostgreSQL(i *ImportOptions) (SchemaImporter, error) {
	return &Postgres{
		ImportOptions: i,
	}, nil
}

// SchemaMutations implements SchemaImporter.
func (p *Postgres) SchemaMutations(ctx context.Context) ([]schemast.Mutator, error) {
	inspectOptions := &schema.InspectOptions{
		Tables: p.tables,
	}
	s, err := p.driver.InspectSchema(ctx, p.driver.SchemaName, inspectOptions)
	if err != nil {
		return nil, err
	}
	tables := s.Tables
	if p.excludedTables != nil {
		tables = nil
		excludedTableNames := make(map[string]bool)
		for _, t := range p.excludedTables {
			excludedTableNames[t] = true
		}
		// filter out tables that are in excludedTables:
		for _, t := range s.Tables {
			if !excludedTableNames[t.Name] {
				tables = append(tables, t)
			}
		}
	}
	// Create a closure that captures the Postgres receiver
	fieldFunc := func(column *schema.Column, table *schema.Table) (ent.Field, error) {
		return p.field(column, table)
	}
	return schemaMutations(fieldFunc, tables, p.ignoreMissingPK)
}

func (p *Postgres) field(column *schema.Column, table *schema.Table) (f ent.Field, err error) {
	name := column.Name
	if p != nil && p.ImportOptions != nil && p.camelCase {
		name = snakeToCamel(column.Name)
	}

	switch typ := column.Type.Type.(type) {
	case *schema.BinaryType:
		f = field.Bytes(name)
	case *schema.BoolType:
		f = field.Bool(name)
	case *schema.DecimalType:
		f = field.Float(name)
	case *schema.EnumType:
		f = field.Enum(name).Values(typ.Values...)
	case *schema.FloatType:
		f = p.convertFloat(typ, name)
	case *schema.IntegerType:
		f = p.convertInteger(typ, name)
	case *schema.JSONType:
		f = field.JSON(name, json.RawMessage{})
	case *schema.StringType:
		f = field.String(name)
	case *schema.TimeType:
		f = field.Time(name)
	case *postgres.SerialType:
		f = p.convertSerial(typ, name)
	case *postgres.UUIDType:
		f = field.UUID(name, uuid.New())
	case *postgres.ArrayType:
		f = p.convertArray(typ, name, column)
		// Don't call applyColumnAttributes for array types since we handle optional directly
		return f, nil
	default:
		// Handle hstore as a user-defined type
		if udt, ok := typ.(*postgres.UserDefinedType); ok && udt.T == "hstore" {
			f = field.JSON(name, json.RawMessage{}).
				SchemaType(map[string]string{
					dialect.Postgres: "jsonb",
				})
		} else if column.Type.Raw == "hstore" {
			// Map hstore to JSONB
			f = field.JSON(name, json.RawMessage{}).
				SchemaType(map[string]string{
					dialect.Postgres: "jsonb",
				})
		} else {
			return nil, fmt.Errorf("entimport: unsupported type %q for column %v", typ, column.Name)
		}
	}

	// Add StorageKey to preserve the original database column name
	f.Descriptor().StorageKey = column.Name

	applyColumnAttributes(f, column, table)
	return f, err
}

// decimal, numeric - user-specified precision, exact up to 131072 digits before the decimal point;
// up to 16383 digits after the decimal point.
// real - 4 bytes variable-precision, inexact 6 decimal digits precision.
// double -	8 bytes	variable-precision, inexact	15 decimal digits precision.
func (p *Postgres) convertFloat(typ *schema.FloatType, name string) (f ent.Field) {
	if typ.T == postgres.TypeReal {
		return field.Float32(name)
	}
	return field.Float(name)
}

func (p *Postgres) convertInteger(typ *schema.IntegerType, name string) (f ent.Field) {
	switch typ.T {
	// smallint - 2 bytes small-range integer -32768 to +32767.
	case "smallint":
		f = field.Int16(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	// integer - 4 bytes typical choice for integer -2147483648 to +2147483647.
	case "integer":
		f = field.Int(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	// bigint - 8 bytes large-range integer -9223372036854775808 to 9223372036854775807.
	case "bigint":
		f = field.Int64(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	}
	return f
}

// smallserial- 2 bytes - small autoincrementing integer 1 to 32767
// serial - 4 bytes autoincrementing integer 1 to 2147483647
// bigserial - 8 bytes large autoincrementing integer	1 to 9223372036854775807
func (p *Postgres) convertSerial(typ *postgres.SerialType, name string) ent.Field {
	switch typ.T {
	case "smallserial":
		return field.Int16(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	case "serial":
		return field.Int(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	case "bigserial":
		return field.Int64(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	default:
		return field.Int(name).
			SchemaType(map[string]string{
				dialect.Postgres: typ.T, // Override Postgres.
			})
	}
}

func (p *Postgres) convertArray(typ *postgres.ArrayType, name string, column *schema.Column) ent.Field {
	f := field.JSON(name, []string{})
	if column.Type.Null {
		return f.Optional()
	}
	return f
}
