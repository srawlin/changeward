DROP INDEX invoices_account_idx;
ALTER TABLE invoices ADD COLUMN legacy_ref text;
