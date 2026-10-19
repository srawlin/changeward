package migrations

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// NewMigration is a migration to write.
type NewMigration struct {
	Name string
	// Up and Down are SQL statements in execution order. A Down entry that
	// starts with "--" is a comment (e.g. an irreversible step).
	Up   []string
	Down []string
	// Header is a comment block placed at the top (provenance).
	Header []string
}

// Writable lists frameworks Changegate can write migrations for.
var Writable = []string{GolangMigrate, Goose, Flyway, Liquibase, Atlas, Prisma, Sqitch, Rails, Django, Alembic}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a description into a migration name.
func Slug(s string) string {
	s = strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "_"), "_")
	if len(s) > 50 {
		s = strings.TrimRight(s[:50], "_")
	}
	if s == "" {
		s = "changegate_promotion"
	}
	return s
}

func sqlBlock(stmts []string) string {
	var b strings.Builder
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		b.WriteString(s)
		if !strings.HasPrefix(s, "--") && !strings.HasSuffix(s, ";") {
			b.WriteString(";")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func header(h []string, prefix string) string {
	var b strings.Builder
	for _, l := range h {
		b.WriteString(prefix + l + "\n")
	}
	return b.String()
}

func maxVersion(set *Set) (maxNum int64, width int, timestamps bool) {
	for _, m := range set.Migrations {
		first := strings.FieldsFunc(m.Version, func(r rune) bool { return r == '.' || r == '_' || r == '-' || r == ':' })
		if len(first) == 0 {
			continue
		}
		n, err := strconv.ParseInt(first[0], 10, 64)
		if err != nil {
			continue
		}
		if len(first[0]) >= 12 {
			timestamps = true
		}
		if n > maxNum {
			maxNum, width = n, len(first[0])
		}
	}
	return
}

func nextSeq(set *Set, now time.Time) string {
	n, width, ts := maxVersion(set)
	if ts || (set != nil && len(set.Migrations) == 0) {
		v := now.UTC().Format("20060102150405")
		if fmt.Sprint(n) >= v {
			v = fmt.Sprint(n + 1)
		}
		return v
	}
	if width == 0 {
		width = 6
	}
	return fmt.Sprintf("%0*d", width, n+1)
}

func writeFile(path, body string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o644)
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Write writes m into dir (relative to root) for framework fw. set, if
// non-nil, is the existing migration set (used for numbering and chaining).
// It returns the files written, relative to root.
func Write(root, fw, dir string, set *Set, m NewMigration, now time.Time) ([]string, error) {
	if set == nil {
		set = &Set{Framework: fw, Dir: dir}
	}
	abs := filepath.Join(root, dir)
	name := Slug(m.Name)
	up, down := sqlBlock(m.Up), sqlBlock(m.Down)
	var files []string
	put := func(rel, body string) error {
		if err := writeFile(filepath.Join(abs, rel), body); err != nil {
			return err
		}
		files = append(files, filepath.Join(dir, rel))
		return nil
	}
	var err error
	switch fw {
	case GolangMigrate:
		v := nextSeq(set, now)
		if err = put(fmt.Sprintf("%s_%s.up.sql", v, name), header(m.Header, "-- ")+up); err == nil {
			err = put(fmt.Sprintf("%s_%s.down.sql", v, name), header(m.Header, "-- ")+down)
		}
	case Goose:
		v := nextSeq(set, now)
		err = put(fmt.Sprintf("%s_%s.sql", v, name), header(m.Header, "-- ")+"-- +goose Up\n-- +goose StatementBegin\n"+up+"-- +goose StatementEnd\n\n-- +goose Down\n-- +goose StatementBegin\n"+down+"-- +goose StatementEnd\n")
	case Flyway:
		n, _, _ := maxVersion(set)
		v := fmt.Sprint(n + 1)
		if err = put(fmt.Sprintf("V%s__%s.sql", v, name), header(m.Header, "-- ")+up); err == nil {
			err = put(fmt.Sprintf("U%s__%s.sql", v, name), header(m.Header, "-- ")+"-- Flyway undo migration (Teams edition runs U files; otherwise apply manually)\n"+down)
		}
	case Liquibase:
		var b strings.Builder
		b.WriteString("--liquibase formatted sql\n")
		b.WriteString(header(m.Header, "-- "))
		fmt.Fprintf(&b, "--changeset changegate:%s-%s\n%s", now.UTC().Format("20060102150405"), name, up)
		for _, d := range strings.Split(strings.TrimSpace(down), "\n") {
			if d != "" {
				b.WriteString("--rollback " + d + "\n")
			}
		}
		err = put(fmt.Sprintf("%s_%s.sql", now.UTC().Format("20060102150405"), name), b.String())
	case Atlas:
		v := now.UTC().Format("20060102150405")
		err = put(fmt.Sprintf("%s_%s.sql", v, name), header(m.Header, "-- ")+up)
	case Prisma:
		v := now.UTC().Format("20060102150405")
		err = put(filepath.Join(v+"_"+name, "migration.sql"), header(m.Header, "-- ")+up)
	case Sqitch:
		if err = put(filepath.Join("deploy", name+".sql"), header(m.Header, "-- ")+"BEGIN;\n"+up+"COMMIT;\n"); err == nil {
			if err = put(filepath.Join("revert", name+".sql"), "BEGIN;\n"+down+"COMMIT;\n"); err == nil {
				plan := filepath.Join(abs, "sqitch.plan")
				f, ferr := os.OpenFile(plan, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
				if ferr != nil {
					return files, ferr
				}
				_, err = fmt.Fprintf(f, "%s %s changegate <changegate@localhost> # %s\n", name, now.UTC().Format(time.RFC3339), m.Name)
				_ = f.Close()
				files = append(files, filepath.Join(dir, "sqitch.plan"))
			}
		}
	case Rails:
		v := now.UTC().Format("20060102150405")
		class := camel(name)
		err = put(fmt.Sprintf("%s_%s.rb", v, name), fmt.Sprintf("%sclass %s < ActiveRecord::Migration[7.1]\n  def up\n    execute <<~SQL\n%s    SQL\n  end\n\n  def down\n    execute <<~SQL\n%s    SQL\n  end\nend\n",
			header(m.Header, "# "), class, indent(up, "      "), indent(down, "      ")))
	case Django:
		n, _, _ := maxVersion(set)
		prev := ""
		for _, x := range set.Migrations {
			if strings.HasPrefix(x.Version, fmt.Sprintf("%04d", n)) {
				prev = x.Version + "_" + x.Name
			}
		}
		app := filepath.Base(filepath.Dir(abs))
		deps := "[]"
		if prev != "" {
			deps = fmt.Sprintf("[(%q, %q)]", app, prev)
		}
		err = put(fmt.Sprintf("%04d_%s.py", n+1, name), fmt.Sprintf("%sfrom django.db import migrations\n\n\nclass Migration(migrations.Migration):\n    dependencies = %s\n\n    operations = [\n        migrations.RunSQL(\n            sql=%s,\n            reverse_sql=%s,\n        ),\n    ]\n",
			header(m.Header, "# "), deps, pyStr(up), pyStr(down)))
	case Alembic:
		rev := randomHex(6)
		downRev := "None"
		if len(set.Migrations) > 0 {
			last := set.Migrations[len(set.Migrations)-1].Version
			if i := strings.IndexByte(last, '-'); i >= 0 {
				last = last[i+1:]
			}
			downRev = fmt.Sprintf("%q", last)
		}
		err = put(fmt.Sprintf("%s_%s.py", rev, name), fmt.Sprintf("%s\"\"\"%s\"\"\"\nfrom alembic import op\n\nrevision = %q\ndown_revision = %s\nbranch_labels = None\ndepends_on = None\n\n\ndef upgrade():\n    op.execute(%s)\n\n\ndef downgrade():\n    op.execute(%s)\n",
			header(m.Header, "# "), m.Name, rev, downRev, pyStr(up), pyStr(down)))
	default:
		return nil, fmt.Errorf("cannot write %s migrations (supported: %s)", fw, strings.Join(Writable, ", "))
	}
	return files, err
}

func camel(s string) string {
	var b strings.Builder
	for _, p := range strings.Split(s, "_") {
		if p == "" {
			continue
		}
		b.WriteString(strings.ToUpper(p[:1]) + p[1:])
	}
	out := b.String()
	if out == "" || (out[0] >= '0' && out[0] <= '9') {
		out = "M" + out
	}
	return out
}

func indent(s, pad string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(pad + l + "\n")
	}
	return b.String()
}

func pyStr(s string) string {
	s = strings.TrimRight(s, "\n")
	if !strings.Contains(s, `"""`) && !strings.HasSuffix(s, `"`) {
		return "\"\"\"\n" + strings.ReplaceAll(s, `\`, `\\`) + "\n\"\"\""
	}
	return fmt.Sprintf("%q", s)
}
