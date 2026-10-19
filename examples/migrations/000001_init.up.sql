CREATE TABLE accounts (
  id bigserial PRIMARY KEY,
  email text NOT NULL UNIQUE,
  plan text NOT NULL DEFAULT 'free',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE invoices (
  id bigserial PRIMARY KEY,
  account_id bigint NOT NULL REFERENCES accounts(id),
  amount_cents bigint NOT NULL,
  legacy_ref text
);
