package pgmem

import (
	"context"
	"crypto/rand"
	"database/sql/driver"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	pgquery "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
	sqlite "modernc.org/sqlite"
)

const dispatchFunctionName = "__pgmem_dispatch_749c9c9e"

// ScalarFunction implements a registered scalar SQL function. Arguments use
// the embedded engine's scalar representations: int64, float64, string,
// []byte, and nil. The returned value must be one of those types or an ordinary
// Go integer or float. bool and time.Time returns are accepted but the embedded
// engine determines how they are materialized.
type ScalarFunction func(arguments ...any) (any, error)

// Function describes one scalar function overload. Arity is the exact number
// of positional arguments for a fixed function and the minimum number for a
// variadic function.
type Function struct {
	Name           string
	Arity          int
	Variadic       bool
	Implementation ScalarFunction
}

type functionOverloads struct {
	fixed    map[int]ScalarFunction
	variadic *variadicFunction
}

type variadicFunction struct {
	minimum        int
	implementation ScalarFunction
}

type functionRegistry struct {
	mu       sync.RWMutex
	bySchema map[string]map[string]*functionOverloads
}

var functionDatabases sync.Map

func init() {
	sqlite.MustRegisterFunction(dispatchFunctionName, &sqlite.FunctionImpl{
		NArgs:  -1,
		Scalar: dispatchScalarFunction,
	})
}

// RegisterFunction registers or atomically replaces one public-schema scalar
// function overload. Registering functions on separate DB values is isolated.
func (db *DB) RegisterFunction(function Function) error {
	if db == nil {
		return ErrClosed
	}
	return db.Public().RegisterFunction(function)
}

// RegisterFunction registers or atomically replaces one scalar function
// overload in this schema. Fixed overloads are selected before a matching
// variadic overload.
func (s *Schema) RegisterFunction(function Function) error {
	if s == nil || s.database == nil {
		return ErrClosed
	}
	if !schemaNamePattern.MatchString(function.Name) || strings.EqualFold(function.Name, dispatchFunctionName) {
		return fmt.Errorf("pgmem: invalid function name %q", function.Name)
	}
	if function.Arity < 0 {
		return fmt.Errorf("pgmem: function %q arity must not be negative", function.Name)
	}
	if function.Implementation == nil {
		return fmt.Errorf("pgmem: function %q implementation must not be nil", function.Name)
	}

	database := s.database
	database.mu.RLock()
	defer database.mu.RUnlock()
	if database.closed {
		return ErrClosed
	}

	name := strings.ToLower(function.Name)
	database.functions.mu.Lock()
	defer database.functions.mu.Unlock()
	functions := database.functions.bySchema[s.name]
	if functions == nil {
		functions = make(map[string]*functionOverloads)
		database.functions.bySchema[s.name] = functions
	}
	overloads := functions[name]
	if overloads == nil {
		overloads = &functionOverloads{fixed: make(map[int]ScalarFunction)}
		functions[name] = overloads
	}
	if function.Variadic {
		overloads.variadic = &variadicFunction{
			minimum:        function.Arity,
			implementation: function.Implementation,
		}
		return nil
	}
	overloads.fixed[function.Arity] = function.Implementation
	return nil
}

func newDispatchID() (string, error) {
	capability := make([]byte, 32)
	if _, err := rand.Read(capability); err != nil {
		return "", fmt.Errorf("generate function-dispatch capability: %w", err)
	}
	return hex.EncodeToString(capability), nil
}

func (db *DB) rewriteRegisteredFunctions(ctx context.Context, defaultSchema, source, statement string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// The substring check is only a fast prefilter. Any possible dispatcher use
	// is still classified structurally below before it can reach the executor.
	if !db.functions.hasAny() && !strings.Contains(strings.ToLower(statement), dispatchFunctionName) {
		return statement, nil
	}
	parsed, err := pgquery.Parse(statement)
	if err != nil {
		return "", fmt.Errorf("pgmem: parse translated statement for registered functions: %w", err)
	}
	changed := false
	for _, rawStatement := range parsed.Stmts {
		if rawStatement == nil || rawStatement.Stmt == nil {
			continue
		}
		if err := rewriteFunctionTree(rawStatement.Stmt.ProtoReflect(), db, defaultSchema, &changed); err != nil {
			return "", &Error{
				Code:      "0A000",
				Message:   "pgmem: internal function dispatcher cannot be called directly",
				Statement: source,
				Cause:     ErrUnsupported,
			}
		}
	}
	if !changed {
		return statement, nil
	}

	rewritten, err := pgquery.Deparse(parsed)
	if err != nil {
		return "", fmt.Errorf("pgmem: deparse registered function call: %w", err)
	}
	return strings.TrimSpace(rewritten), nil
}

func rewriteFunctionTree(message protoreflect.Message, database *DB, defaultSchema string, changed *bool) error {
	var childErr error
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Kind() == protoreflect.MessageKind:
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				if childErr = rewriteFunctionTree(list.Get(index).Message(), database, defaultSchema, changed); childErr != nil {
					return false
				}
			}
		case field.Kind() == protoreflect.MessageKind:
			childErr = rewriteFunctionTree(value.Message(), database, defaultSchema, changed)
		}
		return childErr == nil
	})
	if childErr != nil {
		return childErr
	}

	call, ok := message.Interface().(*pgquery.FuncCall)
	if !ok {
		return nil
	}
	parts, validName := functionNameParts(call)
	if validName && len(parts) > 0 && strings.EqualFold(parts[len(parts)-1], dispatchFunctionName) {
		return ErrUnsupported
	}
	if !plainScalarCall(call) {
		return nil
	}
	schemaName, functionName, ok := calledFunction(call, defaultSchema)
	if !ok || !database.functions.has(schemaName, functionName) {
		return nil
	}

	arguments := make([]*pgquery.Node, 0, len(call.Args)+3)
	arguments = append(arguments,
		pgquery.MakeAConstStrNode(database.dispatchID, -1),
		pgquery.MakeAConstStrNode(schemaName, -1),
		pgquery.MakeAConstStrNode(functionName, -1),
	)
	arguments = append(arguments, call.Args...)
	call.Funcname = []*pgquery.Node{pgquery.MakeStrNode(dispatchFunctionName)}
	call.Args = arguments
	call.Location = -1
	*changed = true
	return nil
}

func plainScalarCall(call *pgquery.FuncCall) bool {
	return call != nil &&
		!call.AggStar &&
		!call.AggDistinct &&
		len(call.AggOrder) == 0 &&
		call.AggFilter == nil &&
		call.Over == nil &&
		!call.FuncVariadic
}

func calledFunction(call *pgquery.FuncCall, defaultSchema string) (string, string, bool) {
	parts, ok := functionNameParts(call)
	if !ok {
		return "", "", false
	}

	var schemaName, functionName string
	switch len(parts) {
	case 1:
		schemaName, functionName = defaultSchema, parts[0]
	case 2:
		schemaName, functionName = parts[0], parts[1]
	default:
		return "", "", false
	}
	if schemaName == "main" {
		schemaName = "public"
	}
	return schemaName, strings.ToLower(functionName), true
}

func functionNameParts(call *pgquery.FuncCall) ([]string, bool) {
	parts := make([]string, 0, len(call.Funcname))
	for _, node := range call.Funcname {
		name := node.GetString_()
		if name == nil {
			return nil, false
		}
		parts = append(parts, name.Sval)
	}
	return parts, true
}

func (r *functionRegistry) has(schemaName, functionName string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, exists := r.bySchema[schemaName][functionName]
	return exists
}

func (r *functionRegistry) hasAny() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.bySchema) > 0
}

func (r *functionRegistry) implementation(schemaName, functionName string, arity int) (ScalarFunction, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	overloads := r.bySchema[schemaName][functionName]
	if overloads == nil {
		return nil, false
	}
	if implementation := overloads.fixed[arity]; implementation != nil {
		return implementation, true
	}
	if overloads.variadic != nil && arity >= overloads.variadic.minimum {
		return overloads.variadic.implementation, true
	}
	return nil, false
}

func dispatchScalarFunction(_ *sqlite.FunctionContext, arguments []driver.Value) (driver.Value, error) {
	if len(arguments) < 3 {
		return nil, fmt.Errorf("pgmem function dispatcher: expected routing arguments")
	}
	databaseID, idOK := arguments[0].(string)
	schemaName, schemaOK := arguments[1].(string)
	functionName, nameOK := arguments[2].(string)
	if !idOK || !schemaOK || !nameOK {
		return nil, fmt.Errorf("pgmem function dispatcher: invalid routing arguments")
	}

	value, exists := functionDatabases.Load(databaseID)
	if !exists {
		return nil, fmt.Errorf("pgmem function %s.%s: database is closed", schemaName, functionName)
	}
	database := value.(*DB)
	implementation, exists := database.functions.implementation(schemaName, functionName, len(arguments)-3)
	if !exists {
		return nil, fmt.Errorf(
			"pgmem function %s.%s: no overload accepts %d arguments",
			schemaName,
			functionName,
			len(arguments)-3,
		)
	}

	callbackArguments := make([]any, len(arguments)-3)
	for index, argument := range arguments[3:] {
		callbackArguments[index] = cloneDriverValue(argument)
	}
	return invokeScalarFunction(schemaName, functionName, implementation, callbackArguments)
}

func invokeScalarFunction(schemaName, functionName string, implementation ScalarFunction, arguments []any) (result driver.Value, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if recoveredError, ok := recovered.(error); ok {
				err = fmt.Errorf("pgmem function %s.%s panicked: %w", schemaName, functionName, recoveredError)
				return
			}
			err = fmt.Errorf("pgmem function %s.%s panicked: %v", schemaName, functionName, recovered)
		}
	}()

	value, callErr := implementation(arguments...)
	if callErr != nil {
		return nil, fmt.Errorf("pgmem function %s.%s failed: %w", schemaName, functionName, callErr)
	}
	value, callErr = normalizeFunctionResult(value)
	if callErr != nil {
		return nil, fmt.Errorf("pgmem function %s.%s returned an invalid value: %w", schemaName, functionName, callErr)
	}
	return value, nil
}

func normalizeFunctionResult(value any) (driver.Value, error) {
	switch typed := value.(type) {
	case nil, int64, float64, bool, string, time.Time:
		return typed, nil
	case []byte:
		return cloneDriverValue(typed), nil
	case int:
		return int64(typed), nil
	case int8:
		return int64(typed), nil
	case int16:
		return int64(typed), nil
	case int32:
		return int64(typed), nil
	case uint:
		if uint64(typed) > math.MaxInt64 {
			return nil, fmt.Errorf("uint value %d overflows int64", typed)
		}
		return int64(typed), nil
	case uint8:
		return int64(typed), nil
	case uint16:
		return int64(typed), nil
	case uint32:
		return int64(typed), nil
	case uint64:
		if typed > math.MaxInt64 {
			return nil, fmt.Errorf("uint64 value %d overflows int64", typed)
		}
		return int64(typed), nil
	case float32:
		return float64(typed), nil
	default:
		return nil, fmt.Errorf("unsupported scalar type %T", value)
	}
}
