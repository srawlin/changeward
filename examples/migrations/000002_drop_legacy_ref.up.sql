ALTER TABLE invoices DROP COLUMN legacy_ref;
CREATE INDEX invoices_account_idx ON invoices(account_id);
