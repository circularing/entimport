package entimport

import (
	"context"
	"encoding/json"
	"fmt"

	"ariga.io/atlas/sql/mysql"
	"ariga.io/atlas/sql/schema"

	"entgo.io/contrib/schemast"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/schema/field"
)

const (
	mTinyInt   = "tinyint"   // MYSQL_TYPE_TINY
	mSmallInt  = "smallint"  // MYSQL_TYPE_SHORT
	mInt       = "int"       // MYSQL_TYPE_LONG
	mMediumInt = "mediumint" // MYSQL_TYPE_INT24
	mBigInt    = "bigint"    // MYSQL_TYPE_LONGLONG
)

// MySQL holds the schema import options and an Atlas inspector instance
type MySQL struct {
	*ImportOptions
}

// NewMySQL - create aמ import structure for MySQL.
func NewMySQL(i *ImportOptions) (*MySQL, error) {
	return &MySQL{
		ImportOptions: i,
	}, nil
}

// SchemaMutations implements SchemaImporter.
func (m *MySQL) SchemaMutations(ctx context.Context) ([]schemast.Mutator, error) {
	inspectOptions := &schema.InspectOptions{
		Tables: m.tables,
	}
	s, err := m.driver.InspectSchema(ctx, m.driver.SchemaName, inspectOptions)
	if err != nil {
		return nil, err
	}
	tables := s.Tables
	if m.excludedTables != nil {
		tables = nil
		excludedTableNames := make(map[string]bool)
		for _, t := range m.excludedTables {
			excludedTableNames[t] = true
		}
		// filter out tables that are in excludedTables:
		for _, t := range s.Tables {
			if !excludedTableNames[t.Name] {
				tables = append(tables, t)
			}
		}
	}
	// Create a closure that captures the MySQL receiver
	fieldFunc := func(column *schema.Column, table *schema.Table) (ent.Field, error) {
		return m.field(column, table)
	}
	return schemaMutations(fieldFunc, tables, m.ignoreMissingPK)
}

func (m *MySQL) field(column *schema.Column, table *schema.Table) (f ent.Field, err error) {
	name := column.Name
	if m != nil && m.ImportOptions != nil && m.camelCase {
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
		f = m.convertFloat(typ, name)
	case *schema.IntegerType:
		f = m.convertInteger(typ, name)
	case *schema.JSONType:
		f = field.JSON(name, json.RawMessage{})
	case *schema.StringType:
		f = field.String(name)
	case *schema.TimeType:
		f = field.Time(name)
	case *mysql.SetType:
		f = field.String(name)
	default:
		return nil, fmt.Errorf("entimport: unsupported type %q for column %v", typ, column.Name)
	}

	// Add StorageKey to preserve the original database column name
	f.Descriptor().StorageKey = column.Name

	applyColumnAttributes(f, column, table)
	return f, err
}

func (m *MySQL) convertFloat(typ *schema.FloatType, name string) (f ent.Field) {
	// A precision from 0 to 23 results in a 4-byte single-precision FLOAT column.
	// A precision from 24 to 53 results in an 8-byte double-precision DOUBLE column:
	// https://dev.mysql.com/doc/refman/8.0/en/floating-point-types.html
	if typ.T == mysql.TypeDouble {
		return field.Float(name)
	}
	return field.Float32(name)
}

func (m *MySQL) convertInteger(typ *schema.IntegerType, name string) (f ent.Field) {
	if typ.Unsigned {
		switch typ.T {
		case mTinyInt:
			f = field.Uint8(name).
				SchemaType(map[string]string{
					dialect.MySQL: typ.T + " unsigned",
				})
		case mSmallInt:
			f = field.Uint16(name).
				SchemaType(map[string]string{
					dialect.MySQL: typ.T + " unsigned",
				})
		case mMediumInt:
			f = field.Uint32(name).
				SchemaType(map[string]string{
					dialect.MySQL: typ.T + " unsigned",
				})
		case mInt:
			f = field.Uint32(name).
				SchemaType(map[string]string{
					dialect.MySQL: typ.T + " unsigned",
				})
		case mBigInt:
			f = field.Uint64(name).
				SchemaType(map[string]string{
					dialect.MySQL: typ.T + " unsigned",
				})
		}
		return f
	}
	switch typ.T {
	case mTinyInt:
		f = field.Int8(name).
			SchemaType(map[string]string{
				dialect.MySQL: typ.T,
			})
	case mSmallInt:
		f = field.Int16(name).
			SchemaType(map[string]string{
				dialect.MySQL: typ.T,
			})
	case mMediumInt:
		f = field.Int32(name).
			SchemaType(map[string]string{
				dialect.MySQL: typ.T,
			})
	case mInt:
		f = field.Int32(name).
			SchemaType(map[string]string{
				dialect.MySQL: typ.T,
			})
	case mBigInt:
		// Int64 is not used on purpose.
		f = field.Int(name).
			SchemaType(map[string]string{
				dialect.MySQL: typ.T,
			})
	}
	return f
}
