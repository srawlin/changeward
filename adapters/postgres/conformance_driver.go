package postgres

import (
	"context"
	"fmt"
	"math/rand"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/srawlin/changeward/conformance"
	"github.com/srawlin/changeward/pkg/adapter"
)

// ConformanceFixture is the schema each conformance target starts from.
const ConformanceFixture = `
CREATE TABLE customers (id serial PRIMARY KEY, email text NOT NULL, name text, score real);
CREATE TABLE orders (
  id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id int REFERENCES customers(id),
  total numeric(12,2) NOT NULL,
  status text NOT NULL DEFAULT 'new',
  tags text[], meta jsonb, blob bytea, during tstzrange,
  placed_at timestamptz NOT NULL DEFAULT '2026-01-01T00:00:00Z',
  total_cents bigint GENERATED ALWAYS AS ((total * 100)::bigint) STORED
);
CREATE TABLE archive (id int PRIMARY KEY, total numeric, created_at date, note varchar(20));
INSERT INTO customers(email, name, score) SELECT 'c' || g || '@example.com', CASE WHEN g % 5 = 0 THEN NULL ELSE 'Customer ' || g END, g / 7.0
  FROM generate_series(1, 25) g;
INSERT INTO orders(customer_id, total, status, tags, meta, blob, during)
  SELECT (g % 25) + 1, g * 1.25, CASE WHEN g % 3 = 0 THEN 'shipped' ELSE 'new' END, ARRAY['t' || g, NULL],
         jsonb_build_object('n', g, 'f', g / 3.0, 'nested', jsonb_build_array(g, 'x')), decode(md5(g::text), 'hex'),
         tstzrange('2026-01-01'::timestamptz + g * interval '1 day', NULL)
  FROM generate_series(1, 60) g;
INSERT INTO archive SELECT g, g * 2, DATE '2020-01-01' + g, 'n' || g FROM generate_series(1, 40) g;
`

// ConformanceDriver returns a conformance driver that creates a fresh
// database (seeded with ConformanceFixture) per check on the server reached
// by adminDSN, and generates a mixed DML/DDL corpus.
func ConformanceDriver(a *Adapter, adminDSN string, seed int64, n int) conformance.Driver {
	return conformance.Driver{
		Adapter: a, Seed: seed, N: n,
		NewTarget: func(ctx context.Context) (adapter.Target, func(), error) {
			c, err := pgx.Connect(ctx, adminDSN)
			if err != nil {
				return adapter.Target{}, nil, err
			}
			defer func() { _ = c.Close(ctx) }()
			name := fmt.Sprintf("cw_conformance_%d", time.Now().UnixNano())
			if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
				return adapter.Target{}, nil, err
			}
			dsn, err := WithDatabase(adminDSN, name)
			if err != nil {
				return adapter.Target{}, nil, err
			}
			tc, err := pgx.Connect(ctx, dsn)
			if err != nil {
				return adapter.Target{}, nil, err
			}
			_, err = tc.Exec(ctx, ConformanceFixture)
			_ = tc.Close(ctx)
			if err != nil {
				return adapter.Target{}, nil, err
			}
			t := adapter.Target{Name: "conformance", Adapter: "postgres", Env: adapter.EnvPersonal, Config: map[string]string{"dsn": dsn}}
			return t, func() {
				a.dropPool(dsn)
				if c, err := pgx.Connect(context.Background(), adminDSN); err == nil {
					_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
					_ = c.Close(context.Background())
				}
			}, nil
		},
		Operations: func(_ context.Context, _ adapter.Target, seed int64, n int) ([]adapter.Operation, error) {
			return GenerateCorpus(seed, n), nil
		},
	}
}

// GenerateCorpus returns n operations that are valid when applied in order
// to ConformanceFixture.
func GenerateCorpus(seed int64, n int) []adapter.Operation {
	rng := rand.New(rand.NewSource(seed))
	var ops []string
	var tables, cols []string
	for i := 0; len(ops) < n; i++ {
		switch rng.Intn(14) {
		case 0, 1:
			ops = append(ops, fmt.Sprintf("UPDATE orders SET status = 's%d', total = total + %d WHERE id %% %d = %d", i, rng.Intn(9)+1, rng.Intn(5)+2, rng.Intn(2)))
		case 2:
			ops = append(ops, fmt.Sprintf("UPDATE customers SET name = %s, score = score * 2 WHERE id = %d", []string{"NULL", fmt.Sprintf("'n%d'", i)}[rng.Intn(2)], rng.Intn(25)+1))
		case 3:
			ops = append(ops, fmt.Sprintf("DELETE FROM archive WHERE id %% %d = %d", rng.Intn(6)+3, rng.Intn(3)))
		case 4:
			base := 1000 + i*10
			ops = append(ops, fmt.Sprintf("INSERT INTO archive SELECT g, g / 3.0, DATE '2021-01-01' + g %% 300, NULL FROM generate_series(%d, %d) g", base, base+rng.Intn(5)))
		case 5:
			ops = append(ops, fmt.Sprintf("INSERT INTO customers(email, name) VALUES ('g%d@example.com', 'G%d'), ('h%d@example.com', NULL)", i, i, i))
		case 6:
			c := fmt.Sprintf("c%d", i)
			cols = append(cols, c)
			ops = append(ops, fmt.Sprintf("ALTER TABLE orders ADD COLUMN %s int DEFAULT %d", c, rng.Intn(100)))
		case 7:
			ops = append(ops, fmt.Sprintf("CREATE INDEX idx%d ON orders(status, id)", i))
		case 8:
			ops = append(ops, fmt.Sprintf(`UPDATE orders SET meta = meta || '{"k%d": %d}', tags = array_append(tags, 'z%d') WHERE id < %d`, i, rng.Intn(50), i, rng.Intn(30)+1))
		case 9:
			t := fmt.Sprintf("t%d", i)
			tables = append(tables, t)
			ops = append(ops, fmt.Sprintf("CREATE TABLE %s (id int PRIMARY KEY, v text NOT NULL); INSERT INTO %s SELECT g, 'v' || g FROM generate_series(1, %d) g", t, t, rng.Intn(20)+1))
		case 10:
			if len(tables) > 0 {
				k := rng.Intn(len(tables))
				ops = append(ops, "DROP TABLE "+tables[k])
				tables = append(tables[:k], tables[k+1:]...)
			}
		case 11:
			if len(cols) > 0 {
				k := rng.Intn(len(cols))
				ops = append(ops, "ALTER TABLE orders DROP COLUMN "+cols[k])
				cols = append(cols[:k], cols[k+1:]...)
			}
		case 12:
			ops = append(ops, fmt.Sprintf("DELETE FROM orders WHERE id = %d", rng.Intn(60)+1))
		case 13:
			if len(tables) > 0 {
				ops = append(ops, "TRUNCATE "+tables[rng.Intn(len(tables))])
			}
		}
	}
	out := make([]adapter.Operation, len(ops))
	for i, o := range ops {
		out[i] = adapter.Operation{Kind: "sql", Payload: o}
	}
	return out
}
