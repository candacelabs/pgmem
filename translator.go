package pgmem

import (
	"context"
	"fmt"
	"strings"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Translator converts PostgreSQL syntax into the internal execution dialect.
// Implementations must be safe for concurrent use.
type Translator interface {
	Translate(ctx context.Context, defaultSchema, statement string) (string, error)
}

type defaultTranslator struct{}

func (defaultTranslator) Translate(ctx context.Context, defaultSchema, statement string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	parsed, err := pgquery.Parse(statement)
	if err != nil {
		return "", &Error{
			Code:      "42601",
			Message:   "invalid PostgreSQL syntax",
			Statement: statement,
			Cause:     err,
		}
	}
	for _, rawStatement := range parsed.Stmts {
		if rawStatement == nil || rawStatement.Stmt == nil {
			continue
		}
		rewritePostgreSQLTree(rawStatement.Stmt.ProtoReflect(), defaultSchema)
	}

	translated, err := pgquery.Deparse(parsed)
	if err != nil {
		return "", fmt.Errorf("deparse PostgreSQL AST: %w", err)
	}
	return strings.TrimSpace(translated), nil
}

func rewritePostgreSQLTree(message protoreflect.Message, defaultSchema string) {
	var createdTable *pgquery.CreateStmt
	switch node := message.Interface().(type) {
	case *pgquery.RangeVar:
		switch {
		case node.Schemaname == "public":
			node.Schemaname = "main"
		case node.Schemaname == "" && defaultSchema == "public":
			node.Schemaname = "main"
		case node.Schemaname == "":
			node.Schemaname = defaultSchema
		}
	case *pgquery.TypeName:
		rewritePostgreSQLType(node)
	case *pgquery.A_Expr:
		if node.Kind == pgquery.A_Expr_Kind_AEXPR_ILIKE {
			node.Kind = pgquery.A_Expr_Kind_AEXPR_LIKE
			for _, name := range node.Name {
				if operator := name.GetString_(); operator != nil {
					switch operator.Sval {
					case "~~*":
						operator.Sval = "~~"
					case "!~~*":
						operator.Sval = "!~~"
					}
				}
			}
		}
	case *pgquery.CreateStmt:
		createdTable = node
	}

	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Kind() == protoreflect.MessageKind:
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				rewritePostgreSQLTree(list.Get(index).Message(), defaultSchema)
			}
		case field.Kind() == protoreflect.MessageKind:
			rewritePostgreSQLTree(value.Message(), defaultSchema)
		}
		return true
	})

	if createdTable != nil && createdTable.Relation != nil {
		for _, element := range createdTable.TableElts {
			clearSameSchemaForeignKeyQualifiers(element.ProtoReflect(), createdTable.Relation.Schemaname)
		}
	}
}

func clearSameSchemaForeignKeyQualifiers(message protoreflect.Message, tableSchema string) {
	// SQLite resolves an unqualified REFERENCES target inside the database
	// containing the table being created and rejects same-database qualifiers.
	// Clear only a qualifier that names that actual database. A target resolved
	// through a different receiver/search path must retain its qualifier so the
	// unsupported cross-schema foreign key fails instead of silently changing
	// meaning.
	if constraint, ok := message.Interface().(*pgquery.Constraint); ok &&
		constraint.Pktable != nil && constraint.Pktable.Schemaname == tableSchema {
		constraint.Pktable.Schemaname = ""
	}

	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Kind() == protoreflect.MessageKind:
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				clearSameSchemaForeignKeyQualifiers(list.Get(index).Message(), tableSchema)
			}
		case field.Kind() == protoreflect.MessageKind:
			clearSameSchemaForeignKeyQualifiers(value.Message(), tableSchema)
		}
		return true
	})
}

func rewritePostgreSQLType(typeName *pgquery.TypeName) {
	if typeName == nil || len(typeName.Names) == 0 {
		return
	}
	lastName := typeName.Names[len(typeName.Names)-1].GetString_()
	if lastName == nil {
		return
	}

	mapped, exists := map[string]string{
		"bigserial":   "integer",
		"bytea":       "blob",
		"int2":        "integer",
		"int4":        "integer",
		"int8":        "integer",
		"jsonb":       "json",
		"serial":      "integer",
		"serial2":     "integer",
		"serial4":     "integer",
		"serial8":     "integer",
		"smallserial": "integer",
		"timestamptz": "timestamp",
		"uuid":        "text",
	}[strings.ToLower(lastName.Sval)]
	if !exists {
		return
	}
	typeName.Names = []*pgquery.Node{{
		Node: &pgquery.Node_String_{String_: &pgquery.String{Sval: mapped}},
	}}
}
