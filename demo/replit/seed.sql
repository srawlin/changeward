-- A small "SaaStr-style" contacts database: companies and their executives.
CREATE TABLE companies (
  id serial PRIMARY KEY,
  name text NOT NULL,
  domain text NOT NULL,
  employees int NOT NULL
);
CREATE TABLE executives (
  id serial PRIMARY KEY,
  company_id int NOT NULL REFERENCES companies(id),
  full_name text NOT NULL,
  email text NOT NULL,
  title text NOT NULL
);
INSERT INTO companies (name, domain, employees)
  SELECT 'Company ' || g, 'company' || g || '.example', 10 + (g * 37) % 5000 FROM generate_series(1, 1196) g;
INSERT INTO executives (company_id, full_name, email, title)
  SELECT 1 + (g % 1196), 'Executive ' || g, 'exec' || g || '@company' || (1 + g % 1196) || '.example',
         (ARRAY['CEO', 'CFO', 'CTO', 'COO', 'VP Sales'])[1 + g % 5]
  FROM generate_series(1, 1206) g;
