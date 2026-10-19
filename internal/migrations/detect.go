// Package migrations detects database migration frameworks in a repository,
// reads their migrations (up and, where the framework has them, down), and
// writes new migrations for `changegate promote`.
package migrations

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Framework names.
const (
	Flyway        = "flyway"
	Liquibase     = "liquibase"
	Atlas         = "atlas"
	Prisma        = "prisma"
	Rails         = "rails"
	Django        = "django"
	Alembic       = "alembic"
	GolangMigrate = "golang-migrate"
	Goose         = "goose"
	Sqitch        = "sqitch"
)

// Frameworks lists every detected framework.
var Frameworks = []string{Flyway, Liquibase, Atlas, Prisma, Rails, Django, Alembic, GolangMigrate, Goose, Sqitch}

// Migration is one migration.
type Migration struct {
	Framework string `json:"framework"`
	Version   string `json:"version"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	DownPath  string `json:"down_path,omitempty"`
	UpSQL     string `json:"-"`
	DownSQL   string `json:"-"`
	HasDown   bool   `json:"has_down"`
	// SQL reports whether Changegate can execute the migration itself.
	// Code-based migrations (Rails, Django, Alembic) need their runtime.
	SQL bool `json:"sql"`
	// NoTransaction is set for migrations that must not run in a transaction.
	NoTransaction bool `json:"no_transaction,omitempty"`
}

// Set is the migrations of one framework in one directory.
type Set struct {
	Framework  string      `json:"framework"`
	Dir        string      `json:"dir"`
	Migrations []Migration `json:"migrations"`
	Notes      []string    `json:"notes,omitempty"`
}

var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, ".venv": true, "venv": true, "__pycache__": true, "dist": true, "build": true, "target": true, ".changegate": true}

var (
	reFlyway    = regexp.MustCompile(`^([VU])(\d+(?:[._]\d+)*)__(.+)\.sql$`)
	reMigrateUp = regexp.MustCompile(`^(\d+)_(.+)\.up\.sql$`)
	reNumbered  = regexp.MustCompile(`^(\d+)_(.+)\.sql$`)
	rePrismaDir = regexp.MustCompile(`^(\d{14})_(.+)$`)
	reRails     = regexp.MustCompile(`^(\d{14})_(.+)\.rb$`)
	reDjango    = regexp.MustCompile(`^(\d{4})_(.+)\.py$`)
	reAlembic   = regexp.MustCompile(`(?m)^revision\s*(?::\s*str\s*)?=\s*['"]([^'"]+)['"]`)
	reAlembicDn = regexp.MustCompile(`(?m)^down_revision\s*(?::[^=]+)?=\s*(?:['"]([^'"]*)['"]|None)`)
)

// Detect scans root for migration sets.
func Detect(root string) ([]Set, error) {
	byDir := map[string][]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // unreadable paths are skipped
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && p != root {
				return filepath.SkipDir
			}
			return nil
		}
		byDir[filepath.Dir(p)] = append(byDir[filepath.Dir(p)], d.Name())
		return nil
	})
	if err != nil {
		return nil, err
	}
	var sets []Set
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, dir := range dirs {
		names := byDir[dir]
		rel, _ := filepath.Rel(root, dir)
		for _, s := range detectDir(dir, rel, names) {
			if len(s.Migrations) > 0 || s.Framework == Liquibase {
				sortMigrations(s.Migrations)
				sets = append(sets, s)
			}
		}
	}
	return sets, nil
}

func has(names []string, n string) bool {
	for _, x := range names {
		if x == n {
			return true
		}
	}
	return false
}

func read(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return string(b)
}

func detectDir(dir, rel string, names []string) []Set {
	var out []Set
	join := func(n string) string { return filepath.Join(rel, n) }
	abs := func(n string) string { return filepath.Join(dir, n) }

	// golang-migrate: NNN_name.up.sql / .down.sql
	mig := Set{Framework: GolangMigrate, Dir: rel}
	for _, n := range names {
		if m := reMigrateUp.FindStringSubmatch(n); m != nil {
			down := strings.TrimSuffix(n, ".up.sql") + ".down.sql"
			x := Migration{Framework: GolangMigrate, Version: m[1], Name: m[2], Path: join(n), UpSQL: read(abs(n)), SQL: true}
			if has(names, down) {
				x.DownPath, x.DownSQL, x.HasDown = join(down), read(abs(down)), true
			}
			mig.Migrations = append(mig.Migrations, x)
		}
	}
	if len(mig.Migrations) > 0 {
		out = append(out, mig)
	}

	// Flyway: V<ver>__name.sql, U<ver>__name.sql
	fw := Set{Framework: Flyway, Dir: rel}
	undo := map[string]string{}
	for _, n := range names {
		if m := reFlyway.FindStringSubmatch(n); m != nil && m[1] == "U" {
			undo[m[2]] = n
		}
	}
	for _, n := range names {
		if m := reFlyway.FindStringSubmatch(n); m != nil && m[1] == "V" {
			x := Migration{Framework: Flyway, Version: m[2], Name: m[3], Path: join(n), UpSQL: read(abs(n)), SQL: true}
			if u, ok := undo[m[2]]; ok {
				x.DownPath, x.DownSQL, x.HasDown = join(u), read(abs(u)), true
			}
			fw.Migrations = append(fw.Migrations, x)
		}
	}
	if len(fw.Migrations) > 0 {
		out = append(out, fw)
	}

	// Atlas: atlas.sum + NNN_name.sql (no down files; Atlas computes downs).
	if has(names, "atlas.sum") {
		at := Set{Framework: Atlas, Dir: rel, Notes: []string{"Atlas migrations have no down files; reversibility is checkpoint-only unless rehearsed with `atlas migrate down`"}}
		for _, n := range names {
			if m := reNumbered.FindStringSubmatch(n); m != nil {
				at.Migrations = append(at.Migrations, Migration{Framework: Atlas, Version: m[1], Name: m[2], Path: join(n), UpSQL: read(abs(n)), SQL: true})
			}
		}
		out = append(out, at)
	}

	// goose: NNN_name.sql with -- +goose Up / Down annotations.
	gs := Set{Framework: Goose, Dir: rel}
	for _, n := range names {
		m := reNumbered.FindStringSubmatch(n)
		if m == nil || strings.HasSuffix(n, ".up.sql") || strings.HasSuffix(n, ".down.sql") {
			continue
		}
		body := read(abs(n))
		if !strings.Contains(body, "+goose Up") {
			continue
		}
		up, down, noTx := splitGoose(body)
		gs.Migrations = append(gs.Migrations, Migration{Framework: Goose, Version: m[1], Name: m[2], Path: join(n), UpSQL: up, DownSQL: down,
			HasDown: strings.TrimSpace(down) != "", SQL: true, NoTransaction: noTx})
	}
	if len(gs.Migrations) > 0 {
		out = append(out, gs)
	}

	// Liquibase formatted SQL: --liquibase formatted sql / --changeset / --rollback.
	lq := Set{Framework: Liquibase, Dir: rel}
	for _, n := range names {
		if !strings.HasSuffix(n, ".sql") {
			continue
		}
		body := read(abs(n))
		if !strings.HasPrefix(strings.TrimSpace(strings.ToLower(body)), "--liquibase formatted sql") {
			continue
		}
		lq.Migrations = append(lq.Migrations, parseLiquibaseSQL(join(n), body)...)
	}
	for _, n := range names {
		if strings.HasPrefix(n, "db.changelog") && (strings.HasSuffix(n, ".xml") || strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".json")) {
			lq.Notes = append(lq.Notes, join(n)+": XML/YAML/JSON changelogs need the Liquibase CLI; configure rehearse.commands.liquibase")
		}
	}
	if len(lq.Migrations) > 0 || len(lq.Notes) > 0 {
		out = append(out, lq)
	}

	// Sqitch: sqitch.plan with deploy/ and revert/.
	if has(names, "sqitch.plan") {
		sq := Set{Framework: Sqitch, Dir: rel}
		for i, change := range parseSqitchPlan(read(abs("sqitch.plan"))) {
			dep := filepath.Join(dir, "deploy", change+".sql")
			rev := filepath.Join(dir, "revert", change+".sql")
			x := Migration{Framework: Sqitch, Version: fmt.Sprintf("%04d", i+1), Name: change, Path: filepath.Join(rel, "deploy", change+".sql"), UpSQL: read(dep), SQL: true}
			if _, err := os.Stat(rev); err == nil {
				x.DownPath, x.DownSQL, x.HasDown = filepath.Join(rel, "revert", change+".sql"), read(rev), true
			}
			sq.Migrations = append(sq.Migrations, x)
		}
		out = append(out, sq)
	}

	// Rails: db/migrate/<ts>_name.rb
	if filepath.Base(dir) == "migrate" && filepath.Base(filepath.Dir(dir)) == "db" {
		rs := Set{Framework: Rails, Dir: rel, Notes: []string{"Rails migrations run through Rails; configure rehearse.commands.rails (e.g. bin/rails db:migrate / db:rollback STEP={n})"}}
		for _, n := range names {
			if m := reRails.FindStringSubmatch(n); m != nil {
				body := read(abs(n))
				rs.Migrations = append(rs.Migrations, Migration{Framework: Rails, Version: m[1], Name: m[2], Path: join(n),
					HasDown: strings.Contains(body, "def change") || strings.Contains(body, "def down")})
			}
		}
		out = append(out, rs)
	}

	// Django: <app>/migrations/NNNN_name.py
	if filepath.Base(dir) == "migrations" && has(names, "__init__.py") {
		dj := Set{Framework: Django, Dir: rel, Notes: []string{"Django migrations run through manage.py; configure rehearse.commands.django"}}
		for _, n := range names {
			if m := reDjango.FindStringSubmatch(n); m != nil {
				body := read(abs(n))
				if !strings.Contains(body, "migrations.Migration") {
					continue
				}
				dj.Migrations = append(dj.Migrations, Migration{Framework: Django, Version: m[1], Name: m[2], Path: join(n), HasDown: !strings.Contains(body, "RunPython(") || strings.Contains(body, "reverse_code")})
			}
		}
		out = append(out, dj)
	}

	// Alembic: versions/*.py with revision / down_revision.
	if filepath.Base(dir) == "versions" {
		al := Set{Framework: Alembic, Dir: rel, Notes: []string{"Alembic migrations run through alembic; configure rehearse.commands.alembic"}}
		var revs []Migration
		parents := map[string]string{}
		for _, n := range names {
			if !strings.HasSuffix(n, ".py") {
				continue
			}
			body := read(abs(n))
			m := reAlembic.FindStringSubmatch(body)
			if m == nil {
				continue
			}
			dn := ""
			if d := reAlembicDn.FindStringSubmatch(body); d != nil {
				dn = d[1]
			}
			parents[m[1]] = dn
			revs = append(revs, Migration{Framework: Alembic, Version: m[1], Name: strings.TrimSuffix(n, ".py"), Path: join(n),
				HasDown: strings.Contains(body, "def downgrade") && !strings.Contains(body, "def downgrade() -> None:\n    pass") && !strings.Contains(body, "def downgrade():\n    pass")})
		}
		al.Migrations = orderAlembic(revs, parents)
		if len(al.Migrations) > 0 {
			out = append(out, al)
		}
	}
	return out
}

// splitGoose splits a goose SQL file into up and down sections.
func splitGoose(body string) (up, down string, noTx bool) {
	var cur *strings.Builder
	var u, d strings.Builder
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "-- +goose Up"):
			cur = &u
			continue
		case strings.HasPrefix(t, "-- +goose Down"):
			cur = &d
			continue
		case strings.HasPrefix(t, "-- +goose NO TRANSACTION"):
			noTx = true
			continue
		case strings.HasPrefix(t, "-- +goose StatementBegin"), strings.HasPrefix(t, "-- +goose StatementEnd"):
			continue
		}
		if cur != nil {
			cur.WriteString(line)
			cur.WriteString("\n")
		}
	}
	return u.String(), d.String(), noTx
}

var reChangeset = regexp.MustCompile(`(?i)^--\s*changeset\s+([^:\s]+):(\S+)(.*)$`)

func parseLiquibaseSQL(path, body string) []Migration {
	var out []Migration
	var cur *Migration
	var up, down strings.Builder
	flush := func() {
		if cur != nil {
			cur.UpSQL, cur.DownSQL = up.String(), down.String()
			cur.HasDown = strings.TrimSpace(cur.DownSQL) != ""
			out = append(out, *cur)
		}
		up.Reset()
		down.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if m := reChangeset.FindStringSubmatch(t); m != nil {
			flush()
			cur = &Migration{Framework: Liquibase, Version: fmt.Sprintf("%s:%03d", path, len(out)+1), Name: m[1] + ":" + m[2], Path: path, SQL: true,
				NoTransaction: strings.Contains(strings.ToLower(m[3]), "runintransaction:false")}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(strings.ToLower(t), "--rollback") {
			down.WriteString(strings.TrimSpace(t[len("--rollback"):]))
			down.WriteString("\n")
			continue
		}
		if strings.HasPrefix(t, "--") {
			continue
		}
		up.WriteString(line)
		up.WriteString("\n")
	}
	flush()
	return out
}

func parseSqitchPlan(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "%") || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "@") {
			continue
		}
		f := strings.Fields(t)
		out = append(out, f[0])
	}
	return out
}

func orderAlembic(revs []Migration, parents map[string]string) []Migration {
	byRev := map[string]Migration{}
	children := map[string][]string{}
	for _, r := range revs {
		byRev[r.Version] = r
		children[parents[r.Version]] = append(children[parents[r.Version]], r.Version)
	}
	var out []Migration
	queue := append([]string(nil), children[""]...)
	seen := map[string]bool{}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		if seen[v] {
			continue
		}
		seen[v] = true
		m := byRev[v]
		m.Version = fmt.Sprintf("%04d-%s", len(out)+1, v)
		out = append(out, m)
		queue = append(queue, children[v]...)
	}
	return out
}

// DetectPrisma finds prisma/migrations directories (one subdirectory per migration).
func DetectPrisma(root string) []Set {
	var out []Set
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // skip unreadable entries
		}
		if skipDirs[d.Name()] && p != root {
			return filepath.SkipDir
		}
		if d.Name() != "migrations" {
			return nil
		}
		if _, err := os.Stat(filepath.Join(p, "migration_lock.toml")); err != nil {
			return nil //nolint:nilerr // not a Prisma directory
		}
		rel, _ := filepath.Rel(root, p)
		s := Set{Framework: Prisma, Dir: rel, Notes: []string{"Prisma migrations have no down migrations; reversibility is checkpoint-only"}}
		entries, _ := os.ReadDir(p)
		for _, e := range entries {
			m := rePrismaDir.FindStringSubmatch(e.Name())
			if !e.IsDir() || m == nil {
				continue
			}
			sqlPath := filepath.Join(p, e.Name(), "migration.sql")
			if _, err := os.Stat(sqlPath); err != nil {
				continue
			}
			s.Migrations = append(s.Migrations, Migration{Framework: Prisma, Version: m[1], Name: m[2], Path: filepath.Join(rel, e.Name(), "migration.sql"), UpSQL: read(sqlPath), SQL: true})
		}
		sortMigrations(s.Migrations)
		out = append(out, s)
		return filepath.SkipDir
	})
	return out
}

// DetectAll runs Detect and DetectPrisma.
func DetectAll(root string) ([]Set, error) {
	sets, err := Detect(root)
	if err != nil {
		return nil, err
	}
	// Prisma's per-migration directories each hold a migration.sql that
	// Detect does not claim; add the Prisma sets.
	return append(sets, DetectPrisma(root)...), nil
}

// versionKey compares versions numerically by dotted/underscored parts.
func versionLess(a, b string) bool {
	pa, pb := splitVersion(a), splitVersion(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			na, ea := strconv.ParseInt(pa[i], 10, 64)
			nb, eb := strconv.ParseInt(pb[i], 10, 64)
			if ea == nil && eb == nil {
				return na < nb
			}
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

func splitVersion(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == '.' || r == '_' || r == ':' || r == '-' })
}

func sortMigrations(ms []Migration) {
	if len(ms) > 0 && ms[0].Framework == Alembic {
		return // already in dependency order
	}
	sort.SliceStable(ms, func(i, j int) bool { return versionLess(ms[i].Version, ms[j].Version) })
}
