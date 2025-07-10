package entimport

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"ariga.io/atlas/sql/schema"
	"github.com/circularing/entimport/internal/mux"

	"entgo.io/contrib/schemast"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"github.com/go-openapi/inflect"
)

const (
	header         = "Code generated " + "by entimport, DO NOT EDIT."
	to     edgeDir = iota
	from
)

var joinTableErr = errors.New("entimport: join tables must be inspected with ref tables - append `tables` flag")

type (
	edgeDir int

	// relOptions are the options passed down to the functions that creates a relation.
	relOptions struct {
		uniqueEdgeToChild    bool
		recursive            bool
		uniqueEdgeFromParent bool
		refName              string
		edgeField            string
	}

	// fieldFunc receives an Atlas column and converts it to an Ent field.
	fieldFunc func(column *schema.Column) (f ent.Field, err error)

	// SchemaImporter is the interface that wraps the SchemaMutations method.
	SchemaImporter interface {
		// SchemaMutations imports a given schema from a data source and returns a list of schemast mutators.
		SchemaMutations(context.Context) ([]schemast.Mutator, error)
	}

	// ImportOptions are the options passed on to every SchemaImporter.
	ImportOptions struct {
		tables          []string
		excludedTables  []string
		schemaPath      string
		driver          *mux.ImportDriver
		ignoreMissingPK bool
	}

	// ImportOption allows for managing import configuration using functional options.
	ImportOption func(*ImportOptions)
)

// WithSchemaPath provides a DSN (data source name) for reading the schema & tables from.
func WithSchemaPath(path string) ImportOption {
	return func(i *ImportOptions) {
		i.schemaPath = path
	}
}

// WithTables limits the schema import to a set of given tables (by all tables are imported)
func WithTables(tables []string) ImportOption {
	return func(i *ImportOptions) {
		i.tables = tables
	}
}

// WithExcludedTables supplies the set of tables to exclude.
func WithExcludedTables(tables []string) ImportOption {
	return func(i *ImportOptions) {
		i.excludedTables = tables
	}
}

// WithDriver provides an import driver to be used by SchemaImporter.
func WithDriver(drv *mux.ImportDriver) ImportOption {
	return func(i *ImportOptions) {
		i.driver = drv
	}
}

// WithIgnoreMissingPrimaryKey provides an option to ignore missing primary keys.
func WithIgnoreMissingPrimaryKey(ignore bool) ImportOption {
	return func(i *ImportOptions) {
		i.ignoreMissingPK = ignore
	}
}

// NewImport calls the relevant data source importer based on a given dialect.
func NewImport(opts ...ImportOption) (SchemaImporter, error) {
	var (
		si  SchemaImporter
		err error
	)
	i := &ImportOptions{}
	for _, apply := range opts {
		apply(i)
	}
	switch i.driver.Dialect {
	case dialect.MySQL:
		si, err = NewMySQL(i)
		if err != nil {
			return nil, err
		}
	case dialect.Postgres:
		si, err = NewPostgreSQL(i)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("entimport: unsupported dialect %q", i.driver.Dialect)
	}
	return si, err
}

// WriteSchema receives a list of mutators, and writes an ent schema to a given location in the file system.
func WriteSchema(mutations []schemast.Mutator, opts ...ImportOption) error {
	i := &ImportOptions{}
	for _, apply := range opts {
		apply(i)
	}
	ctx, err := schemast.Load(i.schemaPath)
	if err != nil {
		return err
	}
	if err = schemast.Mutate(ctx, mutations...); err != nil {
		return err
	}
	return ctx.Print(i.schemaPath, schemast.Header(header))
}

// entEdge creates an edge based on the given params and direction.
func entEdge(nodeName, nodeType string, currentNode *schemast.UpsertSchema, childNode *schemast.UpsertSchema, dir edgeDir, opts relOptions) (e ent.Edge) {
	var desc *edge.Descriptor
	fieldNames := make(map[string]struct{})
	for _, f := range currentNode.Fields {
		fieldNames[f.Descriptor().Name] = struct{}{}
	}
	makeUniqueEdgeName := func(base string) string {
		edgeName := base
		suffix := 2
		exists := func(name string) bool {
			if _, ok := fieldNames[name]; ok {
				return true
			}
			for _, ed := range currentNode.Edges {
				if ed.Descriptor().Name == name {
					return true
				}
			}
			return false
		}
		for exists(edgeName) {
			edgeName = base + "Edge"
			if suffix > 2 {
				edgeName = base + "Edge" + fmt.Sprint(suffix)
			}
			suffix++
		}
		return edgeName
	}
	switch dir {
	case to:
		// For To edges (parent side), use plural name to match the Ref name
		edgeName := inflect.Pluralize(strings.ToLower(typeName(nodeType)))
		edgeName = makeUniqueEdgeName(edgeName)
		e = edge.To(edgeName, ent.Schema.Type)
		desc = e.Descriptor()
		if opts.uniqueEdgeToChild {
			desc.Unique = true
			desc.Name = inflect.Singularize(edgeName)
		}
		if opts.recursive {
			desc.Name = "child_" + desc.Name
		}
	case from:
		// For From edges (child side), use singular name and set Ref to plural name
		edgeName := ""
		if opts.edgeField != "" && strings.HasSuffix(opts.edgeField, "Id") {
			edgeName = inflect.Camelize(strings.TrimSuffix(opts.edgeField, "Id"))
			edgeName = strings.ToLower(edgeName[:1]) + edgeName[1:]
		} else {
			edgeName = inflect.Camelize(nodeName)
			edgeName = strings.ToLower(edgeName[:1]) + edgeName[1:]
		}
		edgeName = makeUniqueEdgeName(edgeName)
		isFieldRequired := false
		if opts.edgeField != "" {
			for _, f := range currentNode.Fields {
				if f.Descriptor().Name == opts.edgeField && !f.Descriptor().Optional {
					isFieldRequired = true
					break
				}
			}
		}
		if isFieldRequired {
			e = edge.From(edgeName, ent.Schema.Type).Required()
		} else {
			e = edge.From(edgeName, ent.Schema.Type)
		}
		desc = e.Descriptor()
		if opts.edgeField != "" {
			for _, f := range currentNode.Fields {
				if f.Descriptor().Name == opts.edgeField && f.Descriptor().Immutable {
					desc.Immutable = true
					break
				}
			}
		}
		if opts.uniqueEdgeFromParent {
			desc.Unique = true
			desc.Name = inflect.Singularize(edgeName)
		}
		if opts.edgeField != "" {
			setEdgeField(e, opts, currentNode)
		}
		refName := opts.refName
		if opts.uniqueEdgeToChild {
			refName = inflect.Singularize(refName)
		}
		desc.RefName = refName
		if opts.recursive {
			desc.Name = "parent_" + desc.Name
			desc.RefName = "child_" + desc.RefName
		}
		// Set .Field() for edge.From if a foreign key exists
		if opts.edgeField != "" {
			desc.Field = opts.edgeField
		}
	}
	desc.Type = typeName(nodeType)
	if dir == from && opts.edgeField != "" {
		desc.Annotations = append(desc.Annotations, entsql.Annotation{
			OnDelete: entsql.SetNull,
		})
	}
	return e
}

// setEdgeField is a function to properly name edge fields.
func setEdgeField(e ent.Edge, opts relOptions, childNode *schemast.UpsertSchema) {
	edgeField := opts.edgeField
	// Check if the field has been renamed to "id" (primary key case)
	for _, f := range childNode.Fields {
		if f.Descriptor().StorageKey == opts.edgeField && f.Descriptor().Name == "id" {
			edgeField = "id"
			break
		}
	}
	// Don't modify field names - just use the original field name
	// The foreign key constraint lookup depends on the exact field name
	e.Descriptor().Field = edgeField
}

// upsertRelation takes 2 nodes and created the edges between them.
func upsertRelation(nodeA *schemast.UpsertSchema, nodeB *schemast.UpsertSchema, opts relOptions) {
	// For O2M relationships:
	// - nodeA (parent) should have edge.To with plural name (e.g., "alarms")
	// - nodeB (child) should have edge.From with singular name (e.g., "user") and Ref to plural name

	// Create edge from parent (nodeA) to child (nodeB) - plural name
	parentToChildOpts := opts
	parentToChildOpts.refName = "" // No Ref for edge.To
	toEdge := entEdge(typeName(nodeB.Name), nodeB.Name, nodeA, nodeB, to, parentToChildOpts)

	// Create edge from child (nodeB) to parent (nodeA) - singular name with Ref
	childToParentOpts := opts
	childToParentOpts.refName = inflect.Pluralize(strings.ToLower(typeName(nodeB.Name))) // Ref to plural name
	fromEdge := entEdge(typeName(nodeA.Name), nodeA.Name, nodeB, nodeA, from, childToParentOpts)

	addEdgeIfNotExists(nodeA, toEdge)
	addEdgeIfNotExists(nodeB, fromEdge)
}

// upsertManyToMany handles the creation of M2M relations.
func upsertManyToMany(mutations map[string]schemast.Mutator, table *schema.Table) error {
	tableA := table.ForeignKeys[0].RefTable
	tableB := table.ForeignKeys[1].RefTable
	var opts relOptions
	if tableA.Name == tableB.Name {
		opts.recursive = true
	}
	nodeA, ok := mutations[tableA.Name].(*schemast.UpsertSchema)
	if !ok {
		return joinTableErr
	}
	nodeB, ok := mutations[tableB.Name].(*schemast.UpsertSchema)
	if !ok {
		return joinTableErr
	}

	// Create bidirectional many-to-many relationship
	// First edge: tableA -> tableB
	opts.refName = typeName(tableB.Name)
	upsertRelation(nodeA, nodeB, opts)

	// Second edge: tableB -> tableA (inverse)
	opts.refName = typeName(tableA.Name)
	upsertRelation(nodeB, nodeA, opts)

	return nil
}

// Note: at this moment ent doesn't support fields on m2m relations.
func isJoinTable(table *schema.Table) bool {
	if table.PrimaryKey == nil || len(table.PrimaryKey.Parts) != 2 || len(table.ForeignKeys) != 2 {
		return false
	}
	// Make sure that the foreign key columns exactly match primary key column.
	for _, fk := range table.ForeignKeys {
		if len(fk.Columns) != 1 {
			return false
		}
		if fk.Columns[0] != table.PrimaryKey.Parts[0].C && fk.Columns[0] != table.PrimaryKey.Parts[1].C {
			return false
		}
	}

	// Check if this is a join table by looking at the table name pattern
	// Common join table patterns: Table1Table2, Table1_Table2, etc.
	tableName := strings.ToLower(table.Name)

	// Check if table name contains multiple entity names (indicating a join table)
	// This is a heuristic to identify join tables even when they have additional columns
	for _, fk := range table.ForeignKeys {
		if fk.RefTable != nil {
			refTableName := strings.ToLower(fk.RefTable.Name)
			if strings.Contains(tableName, refTableName) {
				return true
			}
		}
	}

	// Additional check: if the table has exactly 2 foreign keys and a composite primary key
	// and the table name suggests it's a join table (contains "user" and "circle" for example)
	if len(table.ForeignKeys) == 2 && len(table.PrimaryKey.Parts) == 2 {
		// Check if both foreign key columns are part of the primary key
		fkCols := make(map[string]bool)
		for _, fk := range table.ForeignKeys {
			if len(fk.Columns) == 1 {
				fkCols[fk.Columns[0].Name] = true
			}
		}

		pkCols := make(map[string]bool)
		for _, part := range table.PrimaryKey.Parts {
			pkCols[part.C.Name] = true
		}

		// If all foreign key columns are part of the primary key, it's likely a join table
		if len(fkCols) == 2 && len(pkCols) == 2 {
			for fkCol := range fkCols {
				if !pkCols[fkCol] {
					return false
				}
			}
			return true
		}
	}

	return false
}

func typeName(tableName string) string {
	// For data-related tables, preserve the original name
	if strings.HasSuffix(tableName, "Data") || strings.HasSuffix(tableName, "ActivityData") || strings.HasSuffix(tableName, "Datum") {
		return inflect.Camelize(tableName)
	}
	singular := inflect.Singularize(tableName)
	if singular == tableName {
		return inflect.Camelize(tableName)
	}
	return inflect.Camelize(singular)
}

func tableName(typeName string) string {
	return inflect.Underscore(inflect.Pluralize(typeName))
}

// resolveCompositePrimaryKey handles tables with composite primary keys
// by creating fields for all primary key parts and preserving the structure
func resolveCompositePrimaryKey(field fieldFunc, table *schema.Table) ([]ent.Field, error) {
	if table.PrimaryKey == nil || len(table.PrimaryKey.Parts) <= 1 {
		return nil, fmt.Errorf("entimport: not a composite primary key (table: %v)", table.Name)
	}

	var pkFields []ent.Field
	for _, part := range table.PrimaryKey.Parts {
		f, err := field(part.C)
		if err != nil {
			return nil, err
		}

		// For composite PKs, we keep the original field names and don't rename to "id"
		// This preserves the exact database structure
		desc := f.Descriptor()
		if desc.Name != "id" {
			desc.StorageKey = desc.Name
			// Don't rename to "id" for composite keys to preserve structure
		}

		// Mark all composite key fields as primary keys
		desc.Immutable = true // Primary keys should be immutable

		pkFields = append(pkFields, f)
	}

	return pkFields, nil
}

// resolvePrimaryKey returns the primary key as an ent field for a given table.
func resolvePrimaryKey(field fieldFunc, table *schema.Table, ignoreMissingPK bool) (f ent.Field, err error) {
	if table.PrimaryKey == nil {
		if ignoreMissingPK {
			// If we're ignoring missing primary keys, try to use the first foreign key column as a fallback
			if len(table.ForeignKeys) > 0 && len(table.ForeignKeys[0].Columns) > 0 {
				// Use the first foreign key column as a synthetic primary key
				if f, err = field(table.ForeignKeys[0].Columns[0]); err != nil {
					return nil, err
				}
				return f, nil
			}
			// If no foreign keys either, return nil to indicate no primary key
			return nil, nil
		}
		return nil, fmt.Errorf("entimport: missing primary key (table: %v)", table.Name)
	}
	if len(table.PrimaryKey.Parts) == 0 {
		return nil, fmt.Errorf("entimport: invalid primary key, at least one part must be present (table: %v)", table.Name)
	}

	// Check if this is a composite primary key
	if len(table.PrimaryKey.Parts) > 1 {
		// For composite primary keys, we need to handle them differently
		// This will be handled in upsertNode by calling resolveCompositePrimaryKey
		return nil, fmt.Errorf("entimport: composite primary key detected (table: %v, parts: %d)", table.Name, len(table.PrimaryKey.Parts))
	}

	// Single-part primary key - create field without applying column attributes
	// to avoid setting optional based on column nullability
	col := table.PrimaryKey.Parts[0].C

	// Use the regular field function but then override the optional attribute
	if f, err = field(col); err != nil {
		return nil, err
	}

	// Ensure primary key fields are never optional and preserve the original name
	f.Descriptor().Optional = false
	return f, nil
}

// upsertNode handles the creation of a node from a given table.
func upsertNode(field fieldFunc, table *schema.Table, ignoreMissingPK bool) (*schemast.UpsertSchema, error) {
	upsert := &schemast.UpsertSchema{
		Name: typeName(table.Name),
	}
	if tableName(table.Name) != table.Name {
		upsert.Annotations = []entschema.Annotation{
			entsql.Annotation{Table: table.Name},
		}
	}
	// Use case-insensitive field names for deduplication
	fields := make(map[string]ent.Field)

	// Check if this is a composite primary key first
	if table.PrimaryKey != nil && len(table.PrimaryKey.Parts) > 1 {
		// Handle composite primary key
		compositePKs, err := resolveCompositePrimaryKey(field, table)
		if err != nil {
			return nil, err
		}
		// Add all composite primary key fields
		for _, pkField := range compositePKs {
			fieldKey := strings.ToLower(pkField.Descriptor().Name)
			if _, ok := fields[fieldKey]; !ok {
				fields[fieldKey] = pkField
				upsert.Fields = append(upsert.Fields, pkField)
			}
		}
	} else {
		// Try to resolve single-part primary key
		pk, err := resolvePrimaryKey(field, table, ignoreMissingPK)
		if err != nil {
			return nil, err
		}
		if pk != nil {
			// Single-part primary key
			fieldKey := strings.ToLower(pk.Descriptor().Name)
			if _, ok := fields[fieldKey]; !ok {
				fields[fieldKey] = pk
				upsert.Fields = append(upsert.Fields, pk)
			}
		}
	}
	for _, column := range table.Columns {
		// Skip PK columns to avoid duplication
		skip := false
		if table.PrimaryKey != nil {
			for _, part := range table.PrimaryKey.Parts {
				if part.C.Name == column.Name {
					skip = true
					break
				}
			}
		}
		if skip {
			continue
		}
		fld, err := field(column)
		if err != nil {
			return nil, err
		}
		// Store field with case-insensitive key for lookup
		fieldKey := strings.ToLower(fld.Descriptor().Name)
		if _, ok := fields[fieldKey]; !ok {
			fields[fieldKey] = fld
			upsert.Fields = append(upsert.Fields, fld)
		}
	}
	for _, index := range table.Indexes {
		if index.Unique && len(index.Parts) == 1 {
			fieldKey := strings.ToLower(index.Parts[0].C.Name)
			fld, ok := fields[fieldKey]
			if !ok {
				// If not found by column name, try to find by matching field names
				for _, field := range fields {
					if strings.ToLower(field.Descriptor().StorageKey) == fieldKey {
						fld = field
						ok = true
						break
					}
				}
			}
			if ok {
				fld.Descriptor().Unique = true
			}
		}
	}
	for _, fk := range table.ForeignKeys {
		for _, column := range fk.Columns {
			// FK / Reference column - use case-insensitive lookup
			// First try to find by column name, then by field name
			fieldKey := strings.ToLower(column.Name)
			fld, ok := fields[fieldKey]
			if !ok {
				// If not found by column name, try to find by matching field names
				for _, field := range fields {
					if strings.ToLower(field.Descriptor().StorageKey) == fieldKey {
						fld = field
						ok = true
						break
					}
				}
			}
			if !ok {
				return nil, fmt.Errorf("foreign key for column: %q doesn't exist in referenced table", column.Name)
			}
			// The nullability is already set correctly by applyColumnAttributes
			// based on the actual database column definition
			_ = fld // Use the field to avoid unused variable warning
		}
	}
	return upsert, nil
}

// applyColumnAttributes adds column attributes to a given ent field.
func applyColumnAttributes(f ent.Field, col *schema.Column) {
	desc := f.Descriptor()
	// Don't mark primary key fields as optional, even if the column allows NULL
	// Primary key fields should always be required in Ent
	if desc.Name != "id" {
		desc.Optional = col.Type.Null
	} else {
		// Explicitly ensure id fields are never optional
		desc.Optional = false
	}
	for _, attr := range col.Attrs {
		if a, ok := attr.(*schema.Comment); ok {
			desc.Comment = a.Text
		}
	}
	// Final check: ensure id fields are never optional
	if desc.Name == "id" {
		desc.Optional = false
	}
}

// schemaMutations is in charge of creating all the schema mutations needed for an ent schema.
func schemaMutations(field fieldFunc, tables []*schema.Table, ignoreMissingPK bool) ([]schemast.Mutator, error) {
	mutations := make(map[string]schemast.Mutator)
	joinTables := make(map[string]*schema.Table)
	m2mPairs := make(map[string]struct{}) // Track M2M pairs
	for _, table := range tables {
		if isJoinTable(table) {
			joinTables[table.Name] = table
			// Track both directions for the join table
			if len(table.ForeignKeys) == 2 {
				t1 := table.ForeignKeys[0].RefTable.Name
				t2 := table.ForeignKeys[1].RefTable.Name
				key1 := t1 + ":" + t2
				key2 := t2 + ":" + t1
				m2mPairs[key1] = struct{}{}
				m2mPairs[key2] = struct{}{}
			}
			continue
		}
		node, err := upsertNode(field, table, ignoreMissingPK)
		if err != nil {
			return nil, fmt.Errorf("entimport: issue with table %v: %w", table.Name, err)
		}
		mutations[table.Name] = node
	}
	for _, table := range tables {
		if t, ok := joinTables[table.Name]; ok {
			err := upsertManyToMany(mutations, t)
			if err != nil {
				return nil, err
			}
			continue
		}
		upsertOneToXWithM2MSkip(mutations, table, m2mPairs)
	}
	ml := make([]schemast.Mutator, 0, len(mutations))
	for _, mutator := range mutations {
		ml = append(ml, mutator)
	}
	return ml, nil
}

// O2O Two Types - Child Table has a unique reference (FK) to Parent table
// O2O Same Type - Child Table has a unique reference (FK) to Parent table (itself)
// O2M (The "Many" side, keeps a reference to the "One" side).
// O2M Two Types - Parent has a non-unique reference to Child, and Child has a unique back-reference to Parent
// O2M Same Type - Parent has a non-unique reference to Child, and Child doesn't have a back-reference to Parent.
func upsertOneToX(mutations map[string]schemast.Mutator, table *schema.Table) {
	if table.ForeignKeys == nil {
		return
	}
	idxs := make(map[string]*schema.Index)
	for _, idx := range table.Indexes {
		if len(idx.Parts) != 1 {
			continue
		}
		idxs[idx.Parts[0].C.Name] = idx
	}
	for _, fk := range table.ForeignKeys {
		if len(fk.Columns) != 1 {
			continue
		}
		parent := fk.RefTable
		child := table
		colName := fk.Columns[0].Name
		opts := relOptions{
			uniqueEdgeFromParent: true,
			refName:              tableName(child.Name),
			edgeField:            colName,
		}
		if child.Name == parent.Name {
			opts.recursive = true
		}
		idx, ok := idxs[colName]
		if ok && idx.Unique {
			opts.uniqueEdgeToChild = true
		}
		// If at least one table in the relation does not exist, there is no point to create it.
		parentNode, ok := mutations[parent.Name].(*schemast.UpsertSchema)
		if !ok {
			continue // Skip this foreign key if parent table doesn't exist
		}
		childNode, ok := mutations[child.Name].(*schemast.UpsertSchema)
		if !ok {
			continue // Skip this foreign key if child table doesn't exist
		}

		// Only create the edge from child to parent (the side that has the foreign key)
		// The child table has the foreign key, so it should have edge.From
		childToParentOpts := opts
		childToParentOpts.refName = inflect.Pluralize(strings.ToLower(typeName(child.Name)))
		fromEdge := entEdge(typeName(parent.Name), parent.Name, childNode, parentNode, from, childToParentOpts)
		addEdgeIfNotExists(childNode, fromEdge)

		// Create the edge from parent to child (the side that doesn't have the foreign key)
		// The parent table doesn't have the foreign key, so it should have edge.To
		parentToChildOpts := opts
		parentToChildOpts.refName = "" // No Ref for edge.To
		toEdge := entEdge(typeName(child.Name), child.Name, parentNode, childNode, to, parentToChildOpts)
		addEdgeIfNotExists(parentNode, toEdge)
	}
}

// upsertOneToXWithM2MSkip is like upsertOneToX but skips O2M edges if the pair is already handled as M2M
func upsertOneToXWithM2MSkip(mutations map[string]schemast.Mutator, table *schema.Table, m2mPairs map[string]struct{}) {
	if table.ForeignKeys == nil {
		return
	}
	idxs := make(map[string]*schema.Index)
	for _, idx := range table.Indexes {
		if len(idx.Parts) != 1 {
			continue
		}
		idxs[idx.Parts[0].C.Name] = idx
	}
	for _, fk := range table.ForeignKeys {
		if len(fk.Columns) != 1 {
			continue
		}
		parent := fk.RefTable
		child := table
		// Skip O2M if this pair is handled as M2M (in either direction)
		key1 := parent.Name + ":" + child.Name
		key2 := child.Name + ":" + parent.Name
		if _, ok := m2mPairs[key1]; ok {
			continue
		}
		if _, ok := m2mPairs[key2]; ok {
			continue
		}
		colName := fk.Columns[0].Name
		opts := relOptions{
			uniqueEdgeFromParent: true,
			refName:              tableName(child.Name),
			edgeField:            colName,
		}
		if child.Name == parent.Name {
			opts.recursive = true
		}
		idx, ok := idxs[colName]
		if ok && idx.Unique {
			opts.uniqueEdgeToChild = true
		}
		parentNode, ok := mutations[parent.Name].(*schemast.UpsertSchema)
		if !ok {
			continue
		}
		childNode, ok := mutations[child.Name].(*schemast.UpsertSchema)
		if !ok {
			continue
		}
		// Only create the edge from child to parent (the side that has the foreign key)
		childToParentOpts := opts
		childToParentOpts.refName = inflect.Pluralize(strings.ToLower(typeName(child.Name)))
		fromEdge := entEdge(typeName(parent.Name), parent.Name, childNode, parentNode, from, childToParentOpts)
		addEdgeIfNotExists(childNode, fromEdge)
		// Create the edge from parent to child (the side that doesn't have the foreign key)
		parentToChildOpts := opts
		parentToChildOpts.refName = "" // No Ref for edge.To
		toEdge := entEdge(typeName(child.Name), child.Name, parentNode, childNode, to, parentToChildOpts)
		addEdgeIfNotExists(parentNode, toEdge)
	}
}

// Helper: only add edge if not already present for this type
func addEdgeIfNotExists(schema *schemast.UpsertSchema, newEdge ent.Edge) {
	typeName := newEdge.Descriptor().Type
	for _, e := range schema.Edges {
		if e.Descriptor().Type == typeName {
			return // Edge to this type already exists
		}
	}
	schema.Edges = append(schema.Edges, newEdge)
}
