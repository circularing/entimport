package entimport

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"ariga.io/atlas/sql/schema"
	"github.com/circularing/entimport/internal/mux"

	"entgo.io/contrib/schemast"
	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/index"
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
		joinTableName        string   // For M2M relationships, specify the join table name
		joinTableColumns     []string // For M2M relationships, specify the actual foreign key column names
		extraColumns         []string // For M2M relationships with extra columns, specify the extra column names
	}

	// fieldFunc receives an Atlas column and table and converts it to an Ent field.
	fieldFunc func(column *schema.Column, table *schema.Table) (f ent.Field, err error)

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
		camelCase       bool
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

// WithCamelCase sets the CamelCase option for ImportOptions.
func WithCamelCase(camel bool) ImportOption {
	return func(i *ImportOptions) {
		i.camelCase = camel
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
	if err = ctx.Print(i.schemaPath, schemast.Header(header)); err != nil {
		return err
	}

	// Post-process to add field.ID annotations for junction tables with composite primary keys
	return postProcessCompositeKeys(i.schemaPath)
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

		// For O2M relationships with foreign key field info, create unique edge names
		// This handles cases where one parent has multiple foreign keys to the same child table
		if opts.edgeField != "" {
			// For multiple foreign keys to the same target table, use the FK field name to create unique edge names
			// This handles cases like Circle having both iconId and sleepModeIconId pointing to IconData
			if strings.HasSuffix(opts.edgeField, "Id") {
				fieldBaseName := strings.TrimSuffix(opts.edgeField, "Id")
				// Only use field name if it's a compound name (contains uppercase letters indicating camelCase)
				// AND the field name is NOT just the parent table name (e.g., "alertSource")
				// e.g., "sleepModeIcon" is compound, but "user", "alertSource" are simple table references
				hasUpperCase := false
				for _, r := range fieldBaseName {
					if r >= 'A' && r <= 'Z' {
						hasUpperCase = true
						break
					}
				}

				// Check if the field base name is just the parent table name
				// Get the current node name (parent table) and compare
				isParentTableName := strings.ToLower(fieldBaseName) == strings.ToLower(currentNode.Name) ||
					strings.ToLower(fieldBaseName) == strings.ToLower(typeName(currentNode.Name))

				if hasUpperCase && strings.ToLower(fieldBaseName) != strings.ToLower(typeName(nodeType)) && !isParentTableName {
					// e.g., sleepModeIconId -> sleepmodeicons, but NOT alertSourceId -> alertsources
					edgeName = inflect.Pluralize(strings.ToLower(fieldBaseName))
				}
			}
		}

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
		// Note: Field is already set by setEdgeField above, don't override it
	}
	desc.Type = typeName(nodeType)
	if dir == from && opts.edgeField != "" {
		desc.Annotations = append(desc.Annotations, entsql.Annotation{
			OnDelete: entsql.SetNull,
		})
	}
	// Add StorageKey for M2M relationships to specify the join table name
	// StorageKey can only be used on edge.To(), not on edge.From() with .Ref()
	if opts.joinTableName != "" && dir == to {
		// For M2M edges, we need to set both Table and Columns
		// The Columns field is required by the schemast.Edge function
		// Use actual foreign key column names from the join table
		columns := opts.joinTableColumns
		if len(columns) == 0 {
			// Fallback to default column names if not available
			columns = []string{"id", "id"}
		}

		// Add extra columns if they exist
		if len(opts.extraColumns) > 0 {
			columns = append(columns, opts.extraColumns...)
		}

		desc.StorageKey = &edge.StorageKey{
			Table:   opts.joinTableName,
			Columns: columns,
		}
	}

	return e
}

// setEdgeField is a function to properly name edge fields.
func setEdgeField(e ent.Edge, opts relOptions, childNode *schemast.UpsertSchema) {
	edgeField := opts.edgeField
	// For composite primary keys, we now keep the original column name as the field name
	// So we can use the original column name directly
	e.Descriptor().Field = edgeField
}

// createEdgeSchema creates an edge schema for junction tables to be used with .Through()
func createEdgeSchema(table *schema.Table, field fieldFunc) (*schemast.UpsertSchema, error) {
	if len(table.ForeignKeys) != 2 {
		return nil, fmt.Errorf("edge schema requires exactly 2 foreign keys")
	}

	edgeSchema := &schemast.UpsertSchema{
		Name: typeName(table.Name),
	}

	// Get the foreign key column names for composite primary key
	var fkFields []string
	var fields []ent.Field
	var edges []ent.Edge

	// Create fields for each foreign key
	for _, fk := range table.ForeignKeys {
		if len(fk.Columns) != 1 {
			continue // Skip multi-column foreign keys for now
		}

		col := fk.Columns[0]
		fkFields = append(fkFields, col.Name)

		// Create the field
		f, err := field(col, table)
		if err != nil {
			return nil, err
		}

		desc := f.Descriptor()
		desc.StorageKey = col.Name
		desc.Name = col.Name
		desc.Optional = false
		desc.Immutable = true
		desc.Comment = fmt.Sprintf("Edge schema foreign key field '%s'", col.Name)

		fields = append(fields, f)

		// Create edge back to parent entity
		parentTable := fk.RefTable
		if parentTable != nil {
			// Create unique edge name based on the foreign key column name
			// This handles cases where multiple FKs point to the same table
			var edgeName string
			if strings.HasSuffix(col.Name, "Id") {
				// Use the field name without "Id" suffix as base for edge name
				baseName := strings.TrimSuffix(col.Name, "Id")
				edgeName = strings.ToLower(baseName)
			} else {
				// Fallback to table name if no "Id" suffix
				edgeName = strings.ToLower(typeName(parentTable.Name))
			}

			e := edge.To(edgeName, ent.Schema.Type)
			edgeDesc := e.Descriptor()
			edgeDesc.Type = typeName(parentTable.Name)
			edgeDesc.Required = !desc.Optional
			edgeDesc.Unique = true
			edgeDesc.Immutable = true
			edgeDesc.Field = col.Name

			edges = append(edges, e)
		}
	}

	// Store fields in a map for index processing
	fieldMap := make(map[string]ent.Field)
	for _, f := range fields {
		fieldMap[strings.ToLower(f.Descriptor().Name)] = f
	}

	// Add any extra columns (like timestamps)
	for _, column := range table.Columns {
		isFK := false
		for _, fk := range table.ForeignKeys {
			for _, fkCol := range fk.Columns {
				if fkCol.Name == column.Name {
					isFK = true
					break
				}
			}
			if isFK {
				break
			}
		}

		if !isFK {
			f, err := field(column, table)
			if err != nil {
				return nil, err
			}
			desc := f.Descriptor()
			desc.StorageKey = column.Name
			fields = append(fields, f)
			fieldMap[strings.ToLower(desc.Name)] = f
		}
	}

	edgeSchema.Fields = fields
	edgeSchema.Edges = edges

	// Handle indexes for edge schemas (both unique and non-unique, single and multi-column)
	var entIndexes []ent.Index
	for _, dbIndex := range table.Indexes {
		// Skip primary key indexes (they're handled separately)
		if table.PrimaryKey != nil && dbIndex.Name == table.PrimaryKey.Name {
			continue
		}

		if len(dbIndex.Parts) >= 1 {
			var fieldNames []string
			var validIndex = true

			// Convert database column names to field names
			for _, part := range dbIndex.Parts {
				columnName := part.C.Name
				var fieldName string

				// Find the corresponding field name (handle camelCase conversion)
				found := false
				for _, field := range fieldMap {
					if strings.ToLower(field.Descriptor().StorageKey) == strings.ToLower(columnName) {
						fieldName = field.Descriptor().Name
						found = true
						break
					}
				}

				if !found {
					// Field not found, skip this index
					validIndex = false
					break
				}

				fieldNames = append(fieldNames, fieldName)
			}

			if validIndex && len(fieldNames) > 0 {
				// Create the Ent index
				idx := index.Fields(fieldNames...)
				if dbIndex.Unique {
					idx = idx.Unique()
				}

				// Add storage key if the index name doesn't follow Ent conventions
				if dbIndex.Name != "" {
					idx = idx.StorageKey(dbIndex.Name)
				}

				entIndexes = append(entIndexes, idx)
			}
		}
	}

	// Set the indexes on the edge schema
	if len(entIndexes) > 0 {
		edgeSchema.Indexes = entIndexes
	}

	// Add composite primary key annotation if we have foreign key fields
	if len(fkFields) >= 2 {
		// Create annotations with composite primary key specification
		var annotations []entschema.Annotation

		// Add table annotation
		annotations = append(annotations, entsql.Annotation{
			Table: table.Name,
		})

		// Note: schemast doesn't support field.ID annotation directly
		// For now, we'll document the composite key structure in comments
		// and rely on the field ordering and database constraints to maintain integrity
		// TODO: Consider implementing a custom annotation handler or post-processing step

		edgeSchema.Annotations = annotations

		// Add comments to document the composite primary key structure
		for i, field := range edgeSchema.Fields {
			if i < len(fkFields) {
				desc := field.Descriptor()
				desc.Comment = fmt.Sprintf("Composite primary key field %d/%d: %s", i+1, len(fkFields), fkFields[i])
			}
		}
	} else if len(fkFields) > 0 {
		// Single foreign key - still add table annotation
		edgeSchema.Annotations = []entschema.Annotation{
			entsql.Annotation{
				Table: table.Name,
			},
		}
	}

	return edgeSchema, nil
}

// upsertRelationWithThrough creates a M2M relation using Through() method for join tables with extra columns.
func upsertRelationWithThrough(nodeA *schemast.UpsertSchema, nodeB *schemast.UpsertSchema, joinTableNode *schemast.UpsertSchema, opts relOptions) {
	// Create edge from nodeA to nodeB using Through() method
	edgeName := inflect.Pluralize(strings.ToLower(typeName(nodeB.Name)))
	if opts.recursive {
		edgeName = "child_" + edgeName
	}

	// Create the through edge name (use the edge schema name)
	throughEdgeName := strings.ToLower(inflect.Pluralize(joinTableNode.Name))

	// Create edge.To().Through() relationship
	e := edge.To(edgeName, ent.Schema.Type).Through(throughEdgeName, ent.Schema.Type)
	desc := e.Descriptor()
	desc.Type = typeName(nodeB.Name)

	// Set the correct through type name
	if desc.Through != nil {
		desc.Through.T = joinTableNode.Name
	}

	// Add the edge to nodeA
	addEdgeIfNotExists(nodeA, e)
}

// upsertRelationWithThroughFrom creates a M2M relation using Through() method for the "From" side of the relationship.
func upsertRelationWithThroughFrom(nodeA *schemast.UpsertSchema, nodeB *schemast.UpsertSchema, joinTableNode *schemast.UpsertSchema, opts relOptions) {
	// Create edge from nodeA to nodeB using edge.From().Through() method
	edgeName := inflect.Pluralize(strings.ToLower(typeName(nodeB.Name)))
	if opts.recursive {
		edgeName = "child_" + edgeName
	}

	// Create the through edge name (use the edge schema name)
	throughEdgeName := strings.ToLower(inflect.Pluralize(joinTableNode.Name))

	// Create the ref name (pointing back to nodeA's edge)
	refName := inflect.Pluralize(strings.ToLower(typeName(nodeA.Name)))

	// Create edge.From().Ref().Through() relationship
	e := edge.From(edgeName, ent.Schema.Type).Ref(refName).Through(throughEdgeName, ent.Schema.Type)
	desc := e.Descriptor()
	desc.Type = typeName(nodeB.Name)

	// Set the correct through type name
	if desc.Through != nil {
		desc.Through.T = joinTableNode.Name
	}

	// Add the edge to nodeA
	addEdgeIfNotExists(nodeA, e)
}

// upsertRelationWithThroughSelfRef creates a M2M self-referential relation using Through() method.
func upsertRelationWithThroughSelfRef(nodeA *schemast.UpsertSchema, nodeB *schemast.UpsertSchema, joinTableNode *schemast.UpsertSchema, opts relOptions) {
	// Temporarily disabled - see upsertRelationWithThrough for details
}

// upsertJunctionTableEdges creates edges for junction tables back to parent entities.
func upsertJunctionTableEdges(mutations map[string]schemast.Mutator, table *schema.Table) error {
	if len(table.ForeignKeys) != 2 {
		return fmt.Errorf("junction table %s must have exactly 2 foreign keys", table.Name)
	}

	// Get the junction table node
	joinTableNode, ok := mutations[table.Name].(*schemast.UpsertSchema)
	if !ok {
		return fmt.Errorf("junction table %s not found in mutations", table.Name)
	}

	// Create edges for each foreign key
	for _, fk := range table.ForeignKeys {
		if len(fk.Columns) != 1 {
			continue
		}

		parentTable := fk.RefTable
		if parentTable == nil {
			continue
		}

		// Check if parent entity exists
		if _, ok := mutations[parentTable.Name]; !ok {
			continue
		}

		// Get the foreign key field name
		fkFieldName := fk.Columns[0].Name

		// Create edge name from parent table name
		// For self-referential junction tables, use the field name to make it unique
		edgeName := strings.ToLower(parentTable.Name)
		if table.ForeignKeys[0].RefTable == table.ForeignKeys[1].RefTable {
			// Self-referential junction table - use field name to distinguish edges
			if strings.HasSuffix(fkFieldName, "Id") {
				edgeName = strings.ToLower(strings.TrimSuffix(fkFieldName, "Id"))
			} else {
				edgeName = strings.ToLower(fkFieldName)
			}
		}

		// Create edge from junction table to parent entity
		e := edge.To(edgeName, ent.Schema.Type)
		desc := e.Descriptor()
		desc.Type = typeName(parentTable.Name)
		desc.Unique = true
		desc.Immutable = true // Junction table edges should be immutable
		desc.Field = fkFieldName

		// Check if the field properties and make the edge match
		for _, field := range joinTableNode.Fields {
			if field.Descriptor().Name == fkFieldName {
				// Match optionality
				if field.Descriptor().Optional {
					desc.Required = false
				} else {
					desc.Required = true
				}
				// Match immutability
				desc.Immutable = field.Descriptor().Immutable
				break
			}
		}

		// Add the edge to the junction table
		addEdgeIfNotExists(joinTableNode, e)
	}

	return nil
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
	// Clear join table info for edge.From to avoid StorageKey on edge.From
	childToParentOpts := opts
	childToParentOpts.refName = inflect.Pluralize(strings.ToLower(typeName(nodeB.Name))) // Ref to plural name
	childToParentOpts.joinTableName = ""                                                 // Clear join table name for edge.From
	childToParentOpts.joinTableColumns = nil                                             // Clear join table columns for edge.From
	fromEdge := entEdge(typeName(nodeA.Name), nodeA.Name, nodeB, nodeA, from, childToParentOpts)

	addEdgeIfNotExists(nodeA, toEdge)
	addEdgeIfNotExists(nodeB, fromEdge)
}

// upsertManyToMany handles the creation of M2M relations.
func upsertManyToMany(mutations map[string]schemast.Mutator, table *schema.Table, joinTablesWithExtraColumns map[string]*schema.Table) error {
	tableA := table.ForeignKeys[0].RefTable
	tableB := table.ForeignKeys[1].RefTable

	nodeA, ok := mutations[tableA.Name].(*schemast.UpsertSchema)
	if !ok {
		return joinTableErr
	}
	nodeB, ok := mutations[tableB.Name].(*schemast.UpsertSchema)
	if !ok {
		return joinTableErr
	}

	// Check if this join table has extra columns
	if _, hasExtra := joinTablesWithExtraColumns[table.Name]; hasExtra {
		// For join tables with extra columns, use Through() method
		// This requires the join table to be defined as a regular entity
		joinTableNode, ok := mutations[table.Name].(*schemast.UpsertSchema)
		if !ok {
			return fmt.Errorf("join table with extra columns %s must be defined as a regular entity", table.Name)
		}

		// Create bidirectional many-to-many relationship using Through()
		var opts relOptions
		if tableA.Name == tableB.Name {
			opts.recursive = true
		}
		opts.joinTableName = table.Name
		opts.joinTableColumns = []string{
			table.ForeignKeys[0].Columns[0].Name,
			table.ForeignKeys[1].Columns[0].Name,
		}

		// Check if this is a complex junction table that shouldn't use .Through()
		// Complex junction tables have composite keys with more than just the foreign keys
		isComplexJunction := false
		if table.PrimaryKey != nil && len(table.PrimaryKey.Parts) > 2 {
			isComplexJunction = true
		}

		// For self-referential relationships with complex composite keys, skip .Through()
		// These are better handled as regular entities with O2M relationships
		if tableA.Name == tableB.Name && isComplexJunction {
			// Skip .Through() for complex self-referential junction tables
			// Let them be handled as regular entities in the O2M flow
			return nil
		}

		// Create bidirectional M2M edges using Through() method
		// First direction: nodeA -> nodeB (edge.To)
		opts.refName = typeName(tableB.Name)
		upsertRelationWithThrough(nodeA, nodeB, joinTableNode, opts)

		// Second direction: nodeB -> nodeA (edge.From with Ref)
		// For self-referential relationships, create the reverse edge
		opts.refName = typeName(tableA.Name)
		if tableA.Name == tableB.Name {
			// Self-referential - create parent edge
			upsertRelationWithThroughSelfRef(nodeB, nodeA, joinTableNode, opts)
		} else {
			// Different tables - create normal bidirectional edge
			upsertRelationWithThroughFrom(nodeB, nodeA, joinTableNode, opts)
		}
	} else {
		// For simple join tables, use regular M2M edges
		var opts relOptions
		if tableA.Name == tableB.Name {
			opts.recursive = true
		}
		opts.joinTableName = table.Name
		if len(table.ForeignKeys) == 2 {
			opts.joinTableColumns = []string{
				table.ForeignKeys[0].Columns[0].Name,
				table.ForeignKeys[1].Columns[0].Name,
			}
		}

		// Create bidirectional many-to-many relationship
		opts.refName = typeName(tableB.Name)
		upsertRelation(nodeA, nodeB, opts)

		opts.refName = typeName(tableA.Name)
		upsertRelation(nodeB, nodeA, opts)
	}

	return nil
}

// isJoinTable checks if a table is a join table (M2M relationship)
func isJoinTable(table *schema.Table) bool {
	// Check if the table has exactly 2 foreign keys
	if len(table.ForeignKeys) != 2 {
		return false
	}

	// Collect all foreign key column names
	fkCols := make(map[string]bool)
	for _, fk := range table.ForeignKeys {
		for _, col := range fk.Columns {
			fkCols[col.Name] = true
		}
	}

	// A true junction table must have a composite primary key that includes
	// AT LEAST the foreign key columns (but can have additional columns)
	if table.PrimaryKey != nil && len(table.PrimaryKey.Parts) >= len(fkCols) {
		// Check if all foreign key columns are part of the primary key
		fkInPK := 0
		for _, part := range table.PrimaryKey.Parts {
			if fkCols[part.C.Name] {
				fkInPK++
			}
		}

		// All foreign key columns must be in the primary key for it to be a junction table
		// This ensures it's a proper M2M relationship table, not just an entity with FKs
		if fkInPK == len(fkCols) {
			return true
		}
	}

	return false
}

// HasExtraColumns checks if a join table has additional columns beyond the foreign keys
func HasExtraColumns(table *schema.Table) bool {
	if !isJoinTable(table) {
		return false
	}

	// Count foreign key columns
	fkCols := make(map[string]bool)
	for _, fk := range table.ForeignKeys {
		for _, col := range fk.Columns {
			fkCols[col.Name] = true
		}
	}

	// Count all columns in the table
	totalCols := len(table.Columns)
	fkColCount := len(fkCols)

	// If there are more columns than foreign keys, it has extra columns
	return totalCols > fkColCount
}

// shouldUseEdgeSchema determines if a junction table should be converted to an edge schema
func shouldUseEdgeSchema(table *schema.Table) bool {
	if !isJoinTable(table) {
		return false
	}

	// Junction tables with exactly 2 foreign keys and minimal extra columns
	// are good candidates for edge schemas with .Through() relationships
	if len(table.ForeignKeys) == 2 {
		// Check if the primary key consists only of the foreign key columns
		// (plus possibly extra columns like timestamps)
		fkCols := make(map[string]bool)
		for _, fk := range table.ForeignKeys {
			for _, col := range fk.Columns {
				fkCols[col.Name] = true
			}
		}

		// Count foreign key columns that are part of the primary key
		if table.PrimaryKey != nil {
			fkInPK := 0
			for _, part := range table.PrimaryKey.Parts {
				if fkCols[part.C.Name] {
					fkInPK++
				}
			}
			// All foreign keys should be in the primary key for edge schema conversion
			return fkInPK == len(fkCols)
		}
	}

	return false
}

// Helper: convert snake_case to camelCase, preserve existing camelCase
func snakeToCamel(s string) string {
	// If the string doesn't contain underscores, assume it's already camelCase
	if !strings.Contains(s, "_") {
		return s
	}
	parts := strings.Split(s, "_")
	for i, part := range parts {
		if i == 0 {
			parts[i] = strings.ToLower(part)
		} else {
			parts[i] = strings.Title(strings.ToLower(part))
		}
	}
	return strings.Join(parts, "")
}

func typeName(tableName string) string {
	// For join tables, preserve the exact table name
	if strings.Contains(strings.ToLower(tableName), "user") && strings.Contains(strings.ToLower(tableName), "circle") {
		return inflect.Camelize(tableName)
	}
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
// by creating fields that work within Ent's limitations
func resolveCompositePrimaryKey(field fieldFunc, table *schema.Table) ([]ent.Field, error) {
	if table.PrimaryKey == nil || len(table.PrimaryKey.Parts) <= 1 {
		return nil, fmt.Errorf("entimport: not a composite primary key (table: %v)", table.Name)
	}

	var pkFields []ent.Field

	// Create individual fields for each part of the composite key
	// All fields are regular fields, the composite key is defined in annotations
	for i, part := range table.PrimaryKey.Parts {
		f, err := field(part.C, table)
		if err != nil {
			return nil, err
		}

		desc := f.Descriptor()
		desc.StorageKey = part.C.Name
		desc.Optional = false   // Primary keys should never be optional
		desc.Immutable = true   // Primary keys should be immutable
		desc.Name = part.C.Name // Use the original column name as field name
		desc.Comment = fmt.Sprintf("Composite primary key (part %d/%d): maps to database column '%s'",
			i+1, len(table.PrimaryKey.Parts), part.C.Name)

		pkFields = append(pkFields, f)
	}

	return pkFields, nil
}

// createCompositeKeyAnnotation creates annotations to document composite primary keys
func createCompositeKeyAnnotation(table *schema.Table) []entschema.Annotation {
	if table.PrimaryKey == nil || len(table.PrimaryKey.Parts) <= 1 {
		return nil
	}

	// Create annotations with composite primary key specification
	var annotations []entschema.Annotation

	// Add table annotation
	annotations = append(annotations, entsql.Annotation{
		Table: table.Name,
	})

	// Note: schemast doesn't support field.ID annotation directly
	// For now, we document the composite key structure through field comments
	// and rely on the database constraints to maintain integrity

	return annotations
}

// isCompositePrimaryKey checks if a table has a composite primary key
func isCompositePrimaryKey(table *schema.Table) bool {
	return table.PrimaryKey != nil && len(table.PrimaryKey.Parts) > 1
}

// resolvePrimaryKey returns the primary key as an ent field for a given table.
func resolvePrimaryKey(field fieldFunc, table *schema.Table, ignoreMissingPK bool) (f ent.Field, err error) {
	if table.PrimaryKey == nil {
		if ignoreMissingPK {
			// If we're ignoring missing primary keys, try to use the first foreign key column as a fallback
			if len(table.ForeignKeys) > 0 && len(table.ForeignKeys[0].Columns) > 0 {
				// Use the first foreign key column as a synthetic primary key
				if f, err = field(table.ForeignKeys[0].Columns[0], table); err != nil {
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
	if isCompositePrimaryKey(table) {
		// For composite primary keys, we need to handle them differently
		// This will be handled in upsertNode by calling resolveCompositePrimaryKey
		return nil, fmt.Errorf("entimport: composite primary key detected (table: %v, parts: %d)", table.Name, len(table.PrimaryKey.Parts))
	}

	// Single-part primary key - create field
	col := table.PrimaryKey.Parts[0].C

	// Use the regular field function
	if f, err = field(col, table); err != nil {
		return nil, err
	}

	// Ensure primary key fields are never optional
	f.Descriptor().Optional = false
	return f, nil
}

// upsertNode handles the creation of a node from a given table.
func upsertNode(field fieldFunc, table *schema.Table, ignoreMissingPK bool) (*schemast.UpsertSchema, error) {
	upsert := &schemast.UpsertSchema{
		Name: typeName(table.Name),
	}
	// Use case-insensitive field names for deduplication
	fields := make(map[string]ent.Field)

	// Add table annotation if table name doesn't match the expected name
	if tableName(table.Name) != table.Name {
		upsert.Annotations = []entschema.Annotation{
			entsql.Annotation{Table: table.Name},
		}
	}

	// Add composite key annotation if needed (but avoid duplicates)
	if isCompositePrimaryKey(table) {
		// Only add if we haven't already added a table annotation
		if len(upsert.Annotations) == 0 {
			compositeKeyAnnotations := createCompositeKeyAnnotation(table)
			if compositeKeyAnnotations != nil {
				upsert.Annotations = append(upsert.Annotations, compositeKeyAnnotations...)
			}
		}
	}

	// Handle primary key(s)
	if isCompositePrimaryKey(table) {
		// Composite primary key - create fields for all parts
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
		// Single primary key or no primary key
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

	// Add non-primary key columns
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
		fld, err := field(column, table)
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

	// Handle unique indexes
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

	// Handle indexes (both unique and non-unique, single and multi-column)
	var entIndexes []ent.Index
	for _, dbIndex := range table.Indexes {
		// Skip primary key indexes (they're handled separately)
		if table.PrimaryKey != nil && dbIndex.Name == table.PrimaryKey.Name {
			continue
		}

		// For single-column unique indexes, we already set them as field properties above
		// But we still need to create the index entry for the Indexes() method
		if len(dbIndex.Parts) >= 1 {
			var fieldNames []string
			var validIndex = true

			// Convert database column names to field names
			for _, part := range dbIndex.Parts {
				columnName := part.C.Name
				var fieldName string

				// Find the corresponding field name (handle camelCase conversion)
				found := false
				for _, field := range fields {
					if strings.ToLower(field.Descriptor().StorageKey) == strings.ToLower(columnName) {
						fieldName = field.Descriptor().Name
						found = true
						break
					}
				}

				if !found {
					// Field not found, skip this index
					validIndex = false
					break
				}

				fieldNames = append(fieldNames, fieldName)
			}

			if validIndex && len(fieldNames) > 0 {
				// Create the Ent index
				idx := index.Fields(fieldNames...)
				if dbIndex.Unique {
					idx = idx.Unique()
				}

				// Add storage key if the index name doesn't follow Ent conventions
				// Ent generates index names automatically, but we can preserve the original name
				// by using StorageKey if needed
				if dbIndex.Name != "" {
					idx = idx.StorageKey(dbIndex.Name)
				}

				entIndexes = append(entIndexes, idx)
			}
		}
	}

	// Set the indexes on the schema
	if len(entIndexes) > 0 {
		upsert.Indexes = entIndexes
	}

	// Handle foreign keys
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
func applyColumnAttributes(f ent.Field, col *schema.Column, table *schema.Table) {
	desc := f.Descriptor()

	// Check if this column is part of a primary key
	isPrimaryKey := false
	if table != nil && table.PrimaryKey != nil {
		for _, part := range table.PrimaryKey.Parts {
			if part.C.Name == col.Name {
				isPrimaryKey = true
				break
			}
		}
	}

	// Primary key fields should never be optional
	if isPrimaryKey {
		desc.Optional = false
	} else {
		// For non-primary key fields, set optional based on column nullability
		desc.Optional = col.Type.Null
	}

	// Add column comments
	for _, attr := range col.Attrs {
		if a, ok := attr.(*schema.Comment); ok {
			desc.Comment = a.Text
		}
	}
}

// schemaMutations is in charge of creating all the schema mutations needed for an ent schema.
func schemaMutations(field fieldFunc, tables []*schema.Table, ignoreMissingPK bool) ([]schemast.Mutator, error) {
	mutations := make(map[string]schemast.Mutator)
	joinTables := make(map[string]*schema.Table)
	joinTablesWithExtraColumns := make(map[string]*schema.Table) // Track join tables with extra columns
	edgeSchemaTables := make(map[string]*schema.Table)           // Track tables converted to edge schemas
	m2mPairs := make(map[string]struct{})                        // Track M2M pairs

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

			// Check if this junction table should be converted to an edge schema
			if shouldUseEdgeSchema(table) {
				// Create edge schema instead of regular schema
				edgeSchema, err := createEdgeSchema(table, field)
				if err != nil {
					return nil, fmt.Errorf("entimport: issue creating edge schema for %v: %w", table.Name, err)
				}
				mutations[table.Name] = edgeSchema
				edgeSchemaTables[table.Name] = table
			} else {
				// For all other join tables, generate as regular entities to preserve exact structure
				// This ensures we keep the original columns, indexes, and constraints
				node, err := upsertNode(field, table, ignoreMissingPK)
				if err != nil {
					return nil, fmt.Errorf("entimport: issue with join table %v: %w", table.Name, err)
				}
				mutations[table.Name] = node
			}

			// Mark join tables for later edge processing
			if HasExtraColumns(table) {
				joinTablesWithExtraColumns[table.Name] = table
			}
			continue
		}
		// Generate schema for non-join tables
		node, err := upsertNode(field, table, ignoreMissingPK)
		if err != nil {
			return nil, fmt.Errorf("entimport: issue with table %v: %w", table.Name, err)
		}
		mutations[table.Name] = node
	}

	for _, table := range tables {
		if t, ok := joinTables[table.Name]; ok {
			// Check if this is an edge schema table (should use .Through() edges)
			if _, isEdgeSchema := edgeSchemaTables[table.Name]; isEdgeSchema {
				// Create .Through() edges for edge schema tables
				err := upsertManyToMany(mutations, t, edgeSchemaTables)
				if err != nil {
					return nil, fmt.Errorf("entimport: issue with M2M .Through() relation for edge schema %v: %w", table.Name, err)
				}
				continue
			}

			// For join tables with extra columns, create .Through() edges
			if HasExtraColumns(t) {
				err := upsertManyToMany(mutations, t, joinTablesWithExtraColumns)
				if err != nil {
					return nil, fmt.Errorf("entimport: issue with M2M relation for join table %v: %w", table.Name, err)
				}
				// Create edges for the junction table back to parent entities
				// This is required for .Through() edges to work properly
				err = upsertJunctionTableEdges(mutations, t)
				if err != nil {
					return nil, fmt.Errorf("entimport: issue creating junction table edges for %v: %w", table.Name, err)
				}
				// Skip creating O2M edges for junction tables with .Through() edges
				// The junction table should only be accessible through the .Through() relationship
				continue
			}
			// For join tables without extra columns, create O2M edges since they are generated as regular entities
			// This preserves the exact structure and avoids Ent's automatic join table generation
			upsertOneToXWithM2MSkip(mutations, t, m2mPairs)
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

		// Calculate the ref name to match the parent-to-child edge name
		refName := inflect.Pluralize(strings.ToLower(typeName(child.Name)))
		if colName != "" {
			// For multiple foreign keys to the same target table, use the FK field name to create unique edge names
			// This handles cases like Circle having both iconId and sleepModeIconId pointing to IconData
			if strings.HasSuffix(colName, "Id") {
				fieldBaseName := strings.TrimSuffix(colName, "Id")
				// Only use field name if it's a compound name (contains uppercase letters indicating camelCase)
				// e.g., "sleepModeIcon" is compound, but "user" or "alertsource" is simple
				hasUpperCase := false
				for _, r := range fieldBaseName {
					if r >= 'A' && r <= 'Z' {
						hasUpperCase = true
						break
					}
				}
				if hasUpperCase && strings.ToLower(fieldBaseName) != strings.ToLower(typeName(parent.Name)) {
					// e.g., sleepModeIconId -> sleepmodeicons, but NOT userId -> users (should be alarms)
					refName = inflect.Pluralize(strings.ToLower(fieldBaseName))
				}
			}
		}
		childToParentOpts.refName = refName

		fromEdge := entEdge(typeName(parent.Name), parent.Name, childNode, parentNode, from, childToParentOpts)
		addEdgeIfNotExists(childNode, fromEdge)

		// Create the edge from parent to child (the side that doesn't have the foreign key)
		// The parent table doesn't have the foreign key, so it should have edge.To
		parentToChildOpts := opts
		parentToChildOpts.refName = "" // No Ref for edge.To
		// Pass the foreign key field name to create unique edge names for multiple FKs to the same table
		parentToChildOpts.edgeField = colName
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

		// Calculate the ref name to match the parent-to-child edge name
		refName := inflect.Pluralize(strings.ToLower(typeName(child.Name)))
		if colName != "" {
			// For multiple foreign keys to the same target table, use the FK field name to create unique edge names
			// This handles cases like Circle having both iconId and sleepModeIconId pointing to IconData
			if strings.HasSuffix(colName, "Id") {
				fieldBaseName := strings.TrimSuffix(colName, "Id")
				// Only use field name if it's a compound name (contains uppercase letters indicating camelCase)
				// e.g., "sleepModeIcon" is compound, but "user" or "alertsource" is simple
				hasUpperCase := false
				for _, r := range fieldBaseName {
					if r >= 'A' && r <= 'Z' {
						hasUpperCase = true
						break
					}
				}
				if hasUpperCase && strings.ToLower(fieldBaseName) != strings.ToLower(typeName(parent.Name)) {
					// e.g., sleepModeIconId -> sleepmodeicons, but NOT userId -> users (should be alarms)
					refName = inflect.Pluralize(strings.ToLower(fieldBaseName))
				}
			}
		}
		childToParentOpts.refName = refName

		fromEdge := entEdge(typeName(parent.Name), parent.Name, childNode, parentNode, from, childToParentOpts)
		addEdgeIfNotExists(childNode, fromEdge)
		// Create the edge from parent to child (the side that doesn't have the foreign key)
		parentToChildOpts := opts
		parentToChildOpts.refName = "" // No Ref for edge.To
		// Pass the foreign key field name to create unique edge names for multiple FKs to the same table
		parentToChildOpts.edgeField = colName
		toEdge := entEdge(typeName(child.Name), child.Name, parentNode, childNode, to, parentToChildOpts)
		addEdgeIfNotExists(parentNode, toEdge)
	}
}

// Helper: only add edge if not already present for this type and name
func addEdgeIfNotExists(schema *schemast.UpsertSchema, newEdge ent.Edge) {
	newDesc := newEdge.Descriptor()
	for _, e := range schema.Edges {
		desc := e.Descriptor()
		// Check both type and name to allow multiple edges to the same type with different names
		if desc.Type == newDesc.Type && desc.Name == newDesc.Name {
			return // Edge with this type and name already exists
		}
	}
	schema.Edges = append(schema.Edges, newEdge)
}

// postProcessCompositeKeysSelective adds field.ID annotations only to safe junction tables
func postProcessCompositeKeysSelective(schemaDir string) error {
	// For now, don't add field.ID annotations to avoid template errors
	// The .Through() edges are working correctly without them
	// TODO: Research how to add field.ID annotations without breaking Ent's template system
	return nil
}

// postProcessCompositeKeys adds field.ID annotations to junction table schemas with composite primary keys
func postProcessCompositeKeys(schemaDir string) error {
	return filepath.WalkDir(schemaDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Only process .go files in the schema directory
		if !strings.HasSuffix(path, ".go") || d.IsDir() {
			return nil
		}

		// Extract schema name from filename (e.g., "user_flow_state.go" -> "UserFlowState")
		filename := filepath.Base(path)
		schemaName := typeName(strings.TrimSuffix(filename, ".go"))

		// Read the schema file
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("failed to read schema file %s: %w", path, err)
		}

		// Check if this schema has composite primary key fields
		if !hasCompositeKeyFields(string(content)) {
			return nil
		}

		// Only add field.ID annotations to junction tables to avoid template errors
		if !isJunctionTableSchema(string(content), schemaName) {
			return nil
		}

		// Extract composite key field names from comments
		fieldNames := extractCompositeKeyFields(string(content))
		if len(fieldNames) < 2 {
			return nil
		}

		// Add field.ID annotation to the junction table schema
		updatedContent := addFieldIDAnnotation(string(content), fieldNames)

		// Write back the updated content
		return os.WriteFile(path, []byte(updatedContent), 0644)
	})
}

// hasCompositeKeyFields checks if a schema file has composite primary key fields
func hasCompositeKeyFields(content string) bool {
	// Look for field comments that indicate composite primary keys (both patterns)
	return strings.Contains(content, "Composite primary key field") ||
		strings.Contains(content, "Composite primary key (part")
}

// isJunctionTableSchema checks if a schema represents a junction table that should use field.ID annotation
// According to Ent documentation, field.ID() should ONLY be used for edge schemas used with .Through()
func isJunctionTableSchema(content string, schemaName string) bool {
	// Check if this schema has composite primary key fields
	if !hasCompositeKeyFields(content) {
		return false
	}

	// field.ID() should ONLY be applied to junction tables that are used with .Through() relationships
	// Based on the actual .Through() usage found in the generated schemas, only these should have field.ID():
	throughEnabledTables := map[string]bool{
		"UserCircle":              true, // Used in .Through("usercircles", UserCircle.Type)
		"UserFlowState":           true, // Used in .Through("userflowstates", UserFlowState.Type)
		"UserBannerClosed":        true, // Used in .Through("userbannercloseds", UserBannerClosed.Type)
		"BannerTemplateComponent": true, // Used in .Through("bannertemplatecomponents", BannerTemplateComponent.Type)
	}

	// Only apply field.ID() to schemas that are actually used with .Through()
	// All other junction tables should be regular entities without field.ID()
	return throughEnabledTables[schemaName]
}

// extractCompositeKeyFields extracts field names from composite key field comments
// in the exact order they appear in the Fields() function
func extractCompositeKeyFields(content string) []string {
	// Find the Fields() function and extract field names in order
	fieldsPattern := regexp.MustCompile(`func \([^)]+\) Fields\(\) \[\]ent\.Field \{[^}]+return \[\]ent\.Field\{([^}]+)\}`)
	fieldsMatch := fieldsPattern.FindStringSubmatch(content)
	if len(fieldsMatch) < 2 {
		return nil
	}

	fieldsContent := fieldsMatch[1]

	// Extract field names in order from the Fields() return statement
	// Look for field.Type("name").Immutable().Comment("Composite primary key...")
	fieldDefRe := regexp.MustCompile(`field\.[^(]+\("([^"]+)"\)[^,]*\.Immutable\(\)[^,]*\.Comment\("Composite primary key`)
	matches := fieldDefRe.FindAllStringSubmatch(fieldsContent, -1)

	var fieldNames []string
	for _, match := range matches {
		if len(match) > 1 {
			fieldNames = append(fieldNames, match[1])
		}
	}

	// Fallback: try comment-based extraction with order preservation
	if len(fieldNames) == 0 {
		// Use a map to preserve order based on field numbers in comments
		fieldMap := make(map[int]string)
		maxOrder := 0

		// Pattern 1: "Composite primary key field 1/2: fieldName"
		re1 := regexp.MustCompile(`\.Comment\("Composite primary key field (\d+)/\d+: ([^"]+)"\)`)
		matches1 := re1.FindAllStringSubmatch(content, -1)
		for _, match := range matches1 {
			if len(match) > 2 {
				if order, err := strconv.Atoi(match[1]); err == nil {
					fieldMap[order] = match[2]
					if order > maxOrder {
						maxOrder = order
					}
				}
			}
		}

		// Pattern 2: "Composite primary key (part 1/2): maps to database column 'fieldName'"
		re2 := regexp.MustCompile(`\.Comment\("Composite primary key \(part (\d+)/\d+\): maps to database column '([^']+)'"\)`)
		matches2 := re2.FindAllStringSubmatch(content, -1)
		for _, match := range matches2 {
			if len(match) > 2 {
				if order, err := strconv.Atoi(match[1]); err == nil {
					fieldMap[order] = match[2]
					if order > maxOrder {
						maxOrder = order
					}
				}
			}
		}

		// Build ordered field names
		for i := 1; i <= maxOrder; i++ {
			if name, exists := fieldMap[i]; exists {
				fieldNames = append(fieldNames, name)
			}
		}
	}

	return fieldNames
}

// addFieldIDAnnotation adds field.ID annotation to the Annotations section
func addFieldIDAnnotation(content string, fieldNames []string) string {
	// Build the simple field.ID annotation with parameters in the exact same order as declared in Fields()
	var fieldIDAnnotation string
	if len(fieldNames) == 2 {
		fieldIDAnnotation = fmt.Sprintf(`field.ID("%s", "%s")`, fieldNames[0], fieldNames[1])
	} else if len(fieldNames) > 2 {
		// For more than 2 fields, create the variadic call
		quotedFields := make([]string, len(fieldNames))
		for i, name := range fieldNames {
			quotedFields[i] = fmt.Sprintf(`"%s"`, name)
		}
		fieldIDAnnotation = fmt.Sprintf("field.ID(%s)", strings.Join(quotedFields, ", "))
	}

	// Add import for field if not present
	if !strings.Contains(content, `"entgo.io/ent/schema/field"`) {
		// Find the import block and add field import
		importPattern := regexp.MustCompile(`(import\s*\(\s*(?:[^)]*\n)*\s*)(\))`)
		content = importPattern.ReplaceAllString(content, `${1}	"entgo.io/ent/schema/field"
${2}`)
	}

	// Find the Annotations function and add the field.ID annotation
	// Look for the pattern: return []schema.Annotation{entsql.Annotation{...}}
	// Place field.ID annotation BEFORE other annotations as Ent expects it first
	annotationsPattern := regexp.MustCompile(`(return\s*\[\]schema\.Annotation\{)([^}]*\})(\s*}\s*)`)

	replacement := fmt.Sprintf(`${1}%s, ${2}${3}`, fieldIDAnnotation)
	return annotationsPattern.ReplaceAllString(content, replacement)
}
