package hooks

import "testing"

func TestEvaluate(t *testing.T) {
	targets := []Target{{Name: "app", Adapter: "postgres", Env: "personal", Database: "appdb", Host: "localhost"}}
	cases := []struct {
		cmd  string
		want string
	}{
		{`ls -la`, "allow"},
		{`psql -d appdb -c "SELECT count(*) FROM users"`, "allow"},
		{`psql -d appdb -c "DELETE FROM users"`, "deny"},
		{`psql postgres://u@localhost/appdb -c 'UPDATE t SET a = 1'`, "deny"},
		{`PGPASSWORD=x psql -c "DROP TABLE orders"`, "deny"},
		{`psql -d otherdb -c "DROP TABLE orders"`, "allow"},
		{`psql -d appdb -f migrate.sql`, "deny"},
		{`psql -d appdb -c '\d customers' -c "SELECT * FROM customers WHERE id = 20;"`, "allow"},
		{`psql -d appdb -c '\dt+'`, "allow"},
		{`psql -d appdb -c '\i drop.sql'`, "deny"},
		{`psql -d appdb -c '\! rm -rf /tmp/x'`, "deny"},
		{`psql -d appdb -c '\copy t FROM data.csv'`, "deny"},
		{`psql -d appdb -c '\d t \\ DELETE FROM t'`, "deny"},
		{`cat x.sql | psql -d appdb`, "deny"},
		{`psql -d appdb`, "ask"},
		{`echo hi && psql -d appdb -c "TRUNCATE t"`, "deny"},
		{`changegate exec "DELETE FROM t WHERE id = 1"`, "allow"},
		{`changegate approve p_123`, "deny"},
		{`CHANGEGATE_USER=bob changegate approve p_123 --as bob`, "deny"},
		{`changegate policy init`, "deny"},
		{`changegate trash purge --yes`, "deny"},
		{`kubectl get pods`, "allow"},
		{`kubectl delete pod x`, "ask"},
		{`terraform apply -auto-approve`, "ask"},
		{`terraform plan`, "allow"},
		{`sudo nginx -s reload`, "ask"},
	}
	for _, c := range cases {
		if got := Evaluate(c.cmd, targets); got.Permission != c.want {
			t.Errorf("%s: %s (%s), want %s", c.cmd, got.Permission, got.Reason, c.want)
		}
	}
	withK8s := append(targets, Target{Name: "dev-cluster", Adapter: "kubernetes"}, Target{Name: "infra", Adapter: "terraform"})
	for cmd, want := range map[string]string{"kubectl delete pod x": "deny", "kubectl get pods": "allow", "terraform apply": "deny", "terraform state rm x": "ask"} {
		if got := Evaluate(cmd, withK8s); got.Permission != want {
			t.Errorf("%s with targets: %s, want %s", cmd, got.Permission, want)
		}
	}
	if got := Evaluate(`psql -c "DELETE FROM t"`, nil); got.Permission != "allow" {
		t.Error("no registered targets: psql is not Changegate's business")
	}
}
