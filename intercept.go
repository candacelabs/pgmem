package pgmem

import (
	"context"
	"fmt"
	"sync"
)

// QueryInterceptor can replace a statement with an ad-hoc result. Returning
// handled=false passes the original statement to the next interceptor and,
// eventually, the embedded SQL engine.
//
// Interceptors receive the original PostgreSQL statement and a defensive copy
// of its arguments. They run in registration order, and the first handled
// result wins.
type QueryInterceptor func(ctx context.Context, statement string, arguments []any) (result Result, handled bool, err error)

// Subscription controls one query-interceptor registration.
type Subscription struct {
	once        sync.Once
	unsubscribe func()
}

// Unsubscribe removes the interceptor. It is safe to call more than once and
// concurrently with query execution. An invocation already in progress may
// finish.
func (s *Subscription) Unsubscribe() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.unsubscribe != nil {
			s.unsubscribe()
		}
	})
}

type interceptorRegistration struct {
	id          uint64
	interceptor QueryInterceptor
}

type interceptorRegistry struct {
	mu      sync.RWMutex
	nextID  uint64
	entries []interceptorRegistration
}

// InterceptQueries registers an interceptor for this schema. Interceptors are
// schema-local: registering one on public does not affect a named schema.
//
// InterceptQueries panics when interceptor is nil, matching the usual Go
// registration contract for callbacks that cannot be useful when nil.
func (s *Schema) InterceptQueries(interceptor QueryInterceptor) *Subscription {
	if interceptor == nil {
		panic("pgmem: query interceptor must not be nil")
	}
	if s == nil {
		panic("pgmem: cannot register an interceptor on a nil schema")
	}

	registry := &s.interceptors
	registry.mu.Lock()
	registry.nextID++
	id := registry.nextID
	registry.entries = append(registry.entries, interceptorRegistration{
		id:          id,
		interceptor: interceptor,
	})
	registry.mu.Unlock()

	return &Subscription{unsubscribe: func() {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		for index := range registry.entries {
			if registry.entries[index].id != id {
				continue
			}
			copy(registry.entries[index:], registry.entries[index+1:])
			registry.entries[len(registry.entries)-1] = interceptorRegistration{}
			registry.entries = registry.entries[:len(registry.entries)-1]
			return
		}
	}}
}

func (s *Schema) runInterceptors(ctx context.Context, statement string, arguments []any) (Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, false, err
	}
	if s == nil || s.database == nil || s.database.isClosed() {
		return Result{}, false, ErrClosed
	}

	registry := &s.interceptors
	registry.mu.RLock()
	entries := append([]interceptorRegistration(nil), registry.entries...)
	registry.mu.RUnlock()

	for _, entry := range entries {
		result, handled, err := entry.interceptor(ctx, statement, cloneArguments(arguments))
		if err != nil {
			return Result{}, false, fmt.Errorf("pgmem: intercept query: %w", err)
		}
		if !handled {
			continue
		}
		hasRows := result.Rows != nil
		result = cloneResult(result)
		if hasRows {
			result.RowCount = int64(len(result.Rows))
		}
		if result.Command == "" {
			result.Command = statementCommand(statement)
		}
		return result, true, nil
	}
	return Result{}, false, nil
}

func cloneArguments(arguments []any) []any {
	cloned := make([]any, len(arguments))
	for index, argument := range arguments {
		cloned[index] = cloneDriverValue(argument)
	}
	return cloned
}

func cloneResult(result Result) Result {
	cloned := Result{
		RowCount: result.RowCount,
		Command:  result.Command,
	}
	if result.Rows != nil {
		cloned.Rows = make([]Row, len(result.Rows))
	}
	for rowIndex, row := range result.Rows {
		clonedRow := make(Row, len(row))
		for column, value := range row {
			clonedRow[column] = cloneDriverValue(value)
		}
		cloned.Rows[rowIndex] = clonedRow
	}
	return cloned
}
