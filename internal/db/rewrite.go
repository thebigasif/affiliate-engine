package db

import (
	"context"
	"database/sql/driver"
	"fmt"
)

// rewritePlaceholders converts SQLite-style `?` placeholders to PostgreSQL
// `$N` ordinal placeholders. It is string-, identifier-, comment- and
// dollar-quote aware so a `?` inside a literal never gets rewritten.
//
// This is the ONLY place SQL is transformed between engines (dialect layer
// as the single SQL boundary — risk register item 1).
//
// NOTE: the PostgreSQL jsonb `?` (key-exists) and `?|`/`?&` operators are
// NOT supported in queries routed through this wrapper; use jsonb_contains
// style functions instead if ever needed.
func rewritePlaceholders(q string) string {
	out := make([]byte, 0, len(q)+16)
	n := 0
	i := 0
	for i < len(q) {
		c := q[i]
		switch c {
		case '\'': // string literal; '' is an escaped quote
			j := i + 1
			for j < len(q) {
				if q[j] == '\'' {
					if j+1 < len(q) && q[j+1] == '\'' {
						j += 2
						continue
					}
					break
				}
				j++
			}
			out = append(out, q[i:min(j+1, len(q))]...)
			i = j + 1
		case '"': // quoted identifier
			j := i + 1
			for j < len(q) && q[j] != '"' {
				j++
			}
			out = append(out, q[i:min(j+1, len(q))]...)
			i = j + 1
		case '-': // line comment
			if i+1 < len(q) && q[i+1] == '-' {
				j := i
				for j < len(q) && q[j] != '\n' {
					j++
				}
				out = append(out, q[i:j]...)
				i = j
			} else {
				out = append(out, c)
				i++
			}
		case '/': // block comment
			if i+1 < len(q) && q[i+1] == '*' {
				j := i + 2
				for j+1 < len(q) && !(q[j] == '*' && q[j+1] == '/') {
					j++
				}
				end := min(j+2, len(q))
				out = append(out, q[i:end]...)
				i = end
			} else {
				out = append(out, c)
				i++
			}
		case '$': // dollar-quoted block ($$ ... $$ or $tag$ ... $tag$)
			if tag, ok := dollarTag(q, i); ok {
				j := i + len(tag)
				for j+len(tag) <= len(q) && q[j:j+len(tag)] != tag {
					j++
				}
				end := min(j+len(tag), len(q))
				out = append(out, q[i:end]...)
				i = end
			} else {
				out = append(out, c)
				i++
			}
		case '?':
			n++
			out = append(out, []byte(fmt.Sprintf("$%d", n))...)
			i++
		default:
			out = append(out, c)
			i++
		}
	}
	return string(out)
}

// dollarTag reports the dollar-quote tag starting at q[i] ($$, $fn$, ...).
func dollarTag(q string, i int) (string, bool) {
	if i >= len(q) || q[i] != '$' {
		return "", false
	}
	j := i + 1
	for j < len(q) && (q[j] == '_' || (q[j] >= 'a' && q[j] <= 'z') || (q[j] >= 'A' && q[j] <= 'Z')) {
		j++
	}
	if j < len(q) && q[j] == '$' && j >= i+1 {
		return q[i : j+1], true
	}
	return "", false
}

// --- driver wiring: a connector wrapper that rewrites every query ---

// rewritingConnector wraps a driver.Connector so all SQL passing through
// database/sql gets `?` -> `$N` rewritten before reaching PostgreSQL.
type rewritingConnector struct{ inner driver.Connector }

// NewRewritingConnector wraps a pgx connector with the engine's
// placeholder-rewriting connector, so other openers (the Shopify tenancy
// store, the pooled ledger poster) get identical `?` -> `$N` behavior
// without re-implementing it. Engine call sites are unchanged.
func NewRewritingConnector(inner driver.Connector) driver.Connector {
	return rewritingConnector{inner: inner}
}

func (r rewritingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c, err := r.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return rewritingConn{inner: c}, nil
}

func (r rewritingConnector) Driver() driver.Driver { return rewritingDriver{r.inner.Driver()} }

type rewritingDriver struct{ inner driver.Driver }

func (d rewritingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	if err != nil {
		return nil, err
	}
	return rewritingConn{inner: c}, nil
}

// rewritingConn implements driver.Conn plus the optional interfaces the
// pgx stdlib conn supports (Execer/Queryer/PrepareContext/Ping/...),
// rewriting the query at each entry point.
type rewritingConn struct{ inner driver.Conn }

func (c rewritingConn) Prepare(query string) (driver.Stmt, error) {
	return c.inner.Prepare(rewritePlaceholders(query))
}

func (c rewritingConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if p, ok := c.inner.(driver.ConnPrepareContext); ok {
		return p.PrepareContext(ctx, rewritePlaceholders(query))
	}
	return c.Prepare(query)
}

func (c rewritingConn) Close() error { return c.inner.Close() }
func (c rewritingConn) Begin() (driver.Tx, error) {
	// driver.Conn requires Begin; prefer the tx-aware form when the inner
	// driver implements it (the bare Begin is deprecated since 1.8 but the
	// interface still demands it — delegate, don't call).
	if b, ok := c.inner.(driver.ConnBeginTx); ok {
		return b.BeginTx(context.Background(), driver.TxOptions{})
	}
	//lint:ignore SA1019 driver.Conn mandates Begin; both bundled drivers implement ConnBeginTx, so this fallback is unreachable in practice
	return c.inner.Begin()
}

func (c rewritingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if e, ok := c.inner.(driver.ExecerContext); ok {
		return e.ExecContext(ctx, rewritePlaceholders(query), args)
	}
	return nil, driver.ErrSkip
}

func (c rewritingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if q, ok := c.inner.(driver.QueryerContext); ok {
		return q.QueryContext(ctx, rewritePlaceholders(query), args)
	}
	return nil, driver.ErrSkip
}

func (c rewritingConn) Ping(ctx context.Context) error {
	if p, ok := c.inner.(driver.Pinger); ok {
		return p.Ping(ctx)
	}
	return nil
}

func (c rewritingConn) ResetSession(ctx context.Context) error {
	if r, ok := c.inner.(driver.SessionResetter); ok {
		return r.ResetSession(ctx)
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
