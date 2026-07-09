package policy

import (
	"testing"
	"time"
)

// destructiveCase is one §5.1 catalog conformance row: every rule ID
// gets at least one hit, one safe-pattern pass and one near-miss (the
// dcg corpus style, spec §18).
type destructiveCase struct {
	name    string
	cmd     string
	dialect Dialect
	// home/cwd/root default to the unix test fixture when empty.
	home, cwd, root string
	wantRule        string   // "" = no hit
	wantEnforce     Decision // checked when wantRule != ""
}

// runRuleCases executes catalog conformance cases against fresh
// observe- and enforce-mode engines. All G1 rules observe-flag, so
// the observe expectation is derived: Flag when a rule is expected,
// Allow otherwise.
func runRuleCases(t *testing.T, cases []destructiveCase, kind EventKind, actionType string) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			home := tc.home
			if home == "" {
				home = "/home/u"
			}
			root := tc.root
			if root == "" {
				root = "/home/u/proj"
			}
			cwd := tc.cwd
			if cwd == "" {
				cwd = root
			}
			ev := Event{
				Kind:        kind,
				ActionType:  actionType,
				Target:      tc.cmd,
				Dialect:     tc.dialect,
				Cwd:         cwd,
				ProjectRoot: root,
				SessionID:   "s1",
				Now:         time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
			}
			observe, err := New(Config{Mode: ModeObserve, Home: home})
			if err != nil {
				t.Fatalf("New observe: %v", err)
			}
			enforce, err := New(Config{Mode: ModeEnforce, Home: home})
			if err != nil {
				t.Fatalf("New enforce: %v", err)
			}
			ov, ev2 := observe.Evaluate(ev), enforce.Evaluate(ev)
			if tc.wantRule == "" {
				if ov.RuleID != "" || ev2.RuleID != "" {
					t.Fatalf("want no hit, got observe=%+v enforce=%+v", ov, ev2)
				}
				return
			}
			if ov.RuleID != tc.wantRule || ov.Decision != DecisionFlag {
				t.Errorf("observe = %s/%s, want %s/flag (reason %q)", ov.RuleID, ov.Decision, tc.wantRule, ov.Reason)
			}
			if ev2.RuleID != tc.wantRule || ev2.Decision != tc.wantEnforce {
				t.Errorf("enforce = %s/%s, want %s/%s (reason %q)", ev2.RuleID, ev2.Decision, tc.wantRule, tc.wantEnforce, ev2.Reason)
			}
		})
	}
}

// TestDestructiveRules is the §5.1 conformance table.
func TestDestructiveRules(t *testing.T) {
	t.Parallel()
	cases := []destructiveCase{
		// --- R-101: catastrophic recursive delete ---
		{name: "R-101 hit: rm -rf root", cmd: "rm -rf /", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: rm -fr home", cmd: "rm -fr ~", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: home trailing slash", cmd: "rm -rf ~/", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: $HOME literal", cmd: `rm -rf $HOME`, wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: outside project", cmd: "rm -rf /etc/nginx", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: relative escape", cmd: "rm -rf ../other", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: root-depth wildcard", cmd: "rm -rf /*", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: sudo wrapped", cmd: "sudo rm -rf /var/www", wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: bash -c nested", cmd: `bash -c "rm -rf /"`, wantRule: "R-101", wantEnforce: DecisionDeny},
		{name: "R-101 hit: substitution smuggled", cmd: "echo $(rm -rf /etc/x)", wantRule: "R-101", wantEnforce: DecisionDeny},
		{
			name: "R-101 hit: cmd dialect drive root", cmd: `rd /s /q C:\`, dialect: DialectCmd,
			home: `C:\Users\u`, cwd: `C:\Users\u\proj`, root: `C:\Users\u\proj`,
			wantRule: "R-101", wantEnforce: DecisionDeny,
		},
		{
			name: "R-101 hit: powershell home", cmd: `Remove-Item -Recurse -Force C:\Users\u`, dialect: DialectPowerShell,
			home: `C:\Users\u`, cwd: `C:\Users\u\proj`, root: `C:\Users\u\proj`,
			wantRule: "R-101", wantEnforce: DecisionDeny,
		},
		{
			name: "R-101 hit: cmd del root wildcard", cmd: `del /s /q C:\*`, dialect: DialectCmd,
			home: `C:\Users\u`, cwd: `C:\Users\u\proj`, root: `C:\Users\u\proj`,
			wantRule: "R-101", wantEnforce: DecisionDeny,
		},
		{name: "R-101 safe: tmp allowlisted", cmd: "rm -rf /tmp/build"},
		{name: "R-101 safe: cache allowlisted", cmd: "rm -rf ~/.cache/observer"},
		{name: "R-101 near-miss: inside project", cmd: "rm -rf ./build"},
		{name: "R-101 near-miss: not recursive", cmd: "rm /etc/nginx/nginx.conf"},

		// --- R-102: VCS / project-root delete ---
		{name: "R-102 hit: .git", cmd: "rm -rf .git", wantRule: "R-102", wantEnforce: DecisionAsk},
		{name: "R-102 hit: nested .git path", cmd: "rm -rf ./.git/objects", wantRule: "R-102", wantEnforce: DecisionAsk},
		{name: "R-102 hit: project root itself", cmd: "rm -rf /home/u/proj", wantRule: "R-102", wantEnforce: DecisionAsk},
		{name: "R-102 near-miss: src subdir", cmd: "rm -rf src"},

		// --- R-103: mass-deletion chains ---
		{name: "R-103 hit: find -delete", cmd: "find . -delete", wantRule: "R-103", wantEnforce: DecisionAsk},
		{name: "R-103 hit: find -name -delete", cmd: `find . -name "*.log" -delete`, wantRule: "R-103", wantEnforce: DecisionAsk},
		{name: "R-103 hit: find -exec rm", cmd: `find . -exec rm {} ;`, wantRule: "R-103", wantEnforce: DecisionAsk},
		{name: "R-103 hit: xargs rm", cmd: "git ls-files | xargs rm -f", wantRule: "R-103", wantEnforce: DecisionAsk},
		{name: "R-103 safe: find under tmp", cmd: "find /tmp/scratch -delete"},
		{name: "R-103 near-miss: find only lists", cmd: `find . -name "*.go"`},

		// --- R-104: git discard ---
		{name: "R-104 hit: reset --hard", cmd: "git reset --hard HEAD~1", wantRule: "R-104", wantEnforce: DecisionAsk},
		{name: "R-104 hit: checkout --", cmd: "git checkout -- .", wantRule: "R-104", wantEnforce: DecisionAsk},
		{name: "R-104 hit: clean -fd", cmd: "git clean -fd", wantRule: "R-104", wantEnforce: DecisionAsk},
		{name: "R-104 safe: clean dry run", cmd: "git clean -n"},
		{name: "R-104 safe: clean -fdn dry", cmd: "git clean -fdn"},
		{name: "R-104 near-miss: checkout branch", cmd: "git checkout main"},
		{name: "R-104 near-miss: soft reset", cmd: "git reset --soft HEAD~1"},

		// --- R-110: force push ---
		{name: "R-110 hit: --force main", cmd: "git push --force origin main", wantRule: "R-110", wantEnforce: DecisionDeny},
		{name: "R-110 hit: -f master", cmd: "git push -f origin master", wantRule: "R-110", wantEnforce: DecisionDeny},
		{name: "R-110 hit: release glob", cmd: "git push --force origin release/1.0", wantRule: "R-110", wantEnforce: DecisionDeny},
		{name: "R-110 hit: refspec dst", cmd: "git push -f origin HEAD:main", wantRule: "R-110", wantEnforce: DecisionDeny},
		{name: "R-110 hit: no refspec conservative", cmd: "git push --force", wantRule: "R-110", wantEnforce: DecisionDeny},
		{name: "R-110 safe: force-with-lease", cmd: "git push --force-with-lease origin main"},
		{name: "R-110 near-miss: feature branch", cmd: "git push -f origin feature-x"},
		{name: "R-110 near-miss: plain push", cmd: "git push origin main"},

		// --- R-111: protected ref deletion ---
		{name: "R-111 hit: branch -D main", cmd: "git branch -D main", wantRule: "R-111", wantEnforce: DecisionAsk},
		{name: "R-111 hit: push :ref", cmd: "git push origin :main", wantRule: "R-111", wantEnforce: DecisionAsk},
		{name: "R-111 hit: push --delete", cmd: "git push --delete origin master", wantRule: "R-111", wantEnforce: DecisionAsk},
		{name: "R-111 near-miss: feature branch", cmd: "git branch -D feature-x"},
		{name: "R-111 near-miss: lowercase -d merged-only", cmd: "git branch -d main"},

		// --- R-120: destructive SQL ---
		{name: "R-120 hit: psql DROP", cmd: `psql -c "DROP TABLE users"`, wantRule: "R-120", wantEnforce: DecisionDeny},
		{name: "R-120 hit: mysql DELETE no WHERE", cmd: `mysql -e "DELETE FROM accounts"`, wantRule: "R-120", wantEnforce: DecisionDeny},
		{name: "R-120 hit: sqlite3 positional", cmd: `sqlite3 app.db "DROP TABLE users"`, wantRule: "R-120", wantEnforce: DecisionDeny},
		{name: "R-120 hit: heredoc TRUNCATE", cmd: "psql <<SQL\nTRUNCATE TABLE logs;\nSQL", wantRule: "R-120", wantEnforce: DecisionDeny},
		{name: "R-120 near-miss: DELETE with WHERE", cmd: `mysql -e "DELETE FROM accounts WHERE id = 5"`},
		{name: "R-120 near-miss: SELECT", cmd: `psql -c "SELECT * FROM users"`},

		// --- R-130: cloud destroy ---
		{name: "R-130 hit: terraform destroy", cmd: "terraform destroy", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: apply -auto-approve", cmd: "terraform apply -auto-approve", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: aws s3 rm recursive", cmd: "aws s3 rm s3://bucket --recursive", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: terminate instances", cmd: "aws ec2 terminate-instances --instance-ids i-123", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: gcloud delete", cmd: "gcloud compute instances delete vm-1", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: kubectl --all", cmd: "kubectl delete pods --all", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: kubectl namespace", cmd: "kubectl delete namespace prod", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 hit: helm uninstall", cmd: "helm uninstall my-release", wantRule: "R-130", wantEnforce: DecisionAsk},
		{name: "R-130 near-miss: plain apply", cmd: "terraform apply"},
		{name: "R-130 near-miss: terraform plan", cmd: "terraform plan"},
		{name: "R-130 near-miss: single pod", cmd: "kubectl delete pod my-pod"},
		{name: "R-130 near-miss: aws s3 ls", cmd: "aws s3 ls s3://bucket"},

		// --- R-140: registry publish/yank ---
		{name: "R-140 hit: npm publish", cmd: "npm publish", wantRule: "R-140", wantEnforce: DecisionAsk},
		{name: "R-140 hit: npm unpublish", cmd: "npm unpublish my-pkg", wantRule: "R-140", wantEnforce: DecisionAsk},
		{name: "R-140 hit: cargo publish", cmd: "cargo publish", wantRule: "R-140", wantEnforce: DecisionAsk},
		{name: "R-140 hit: twine upload", cmd: "twine upload dist/*", wantRule: "R-140", wantEnforce: DecisionAsk},
		{name: "R-140 hit: gem push", cmd: "gem push pkg-1.0.gem", wantRule: "R-140", wantEnforce: DecisionAsk},
		{name: "R-140 near-miss: npm install", cmd: "npm install"},
		{name: "R-140 near-miss: cargo build", cmd: "cargo build --release"},

		// --- R-141: bulk permission change ---
		{name: "R-141 hit: chmod -R 777", cmd: "chmod -R 777 /var/www", wantRule: "R-141", wantEnforce: DecisionAsk},
		{name: "R-141 hit: chmod -R 0777 inside", cmd: "chmod -R 0777 .", wantRule: "R-141", wantEnforce: DecisionAsk},
		{name: "R-141 hit: chown -R outside", cmd: "chown -R bob /etc/app", wantRule: "R-141", wantEnforce: DecisionAsk},
		{name: "R-141 near-miss: chmod -R 755", cmd: "chmod -R 755 ."},
		{name: "R-141 near-miss: no recursion", cmd: "chmod 777 build/script.sh"},
		{name: "R-141 near-miss: chown inside project", cmd: "chown -R bob ./src"},

		// --- R-142: disk/device ops ---
		{name: "R-142 hit: mkfs", cmd: "mkfs.ext4 /dev/sda1", wantRule: "R-142", wantEnforce: DecisionDeny},
		{name: "R-142 hit: dd to device", cmd: "dd if=/dev/zero of=/dev/sda bs=1M", wantRule: "R-142", wantEnforce: DecisionDeny},
		{name: "R-142 hit: diskpart", cmd: "diskpart /s wipe.txt", dialect: DialectCmd, home: `C:\Users\u`, cwd: `C:\Users\u\proj`, root: `C:\Users\u\proj`, wantRule: "R-142", wantEnforce: DecisionDeny},
		{name: "R-142 hit: cmd format", cmd: "format d:", dialect: DialectCmd, home: `C:\Users\u`, cwd: `C:\Users\u\proj`, root: `C:\Users\u\proj`, wantRule: "R-142", wantEnforce: DecisionDeny},
		{name: "R-142 hit: Format-Volume", cmd: "Format-Volume -DriveLetter D", dialect: DialectPowerShell, home: `C:\Users\u`, cwd: `C:\Users\u\proj`, root: `C:\Users\u\proj`, wantRule: "R-142", wantEnforce: DecisionDeny},
		{name: "R-142 near-miss: dd to file", cmd: "dd if=/dev/zero of=./disk.img bs=1M"},
		{name: "R-142 near-miss: posix format script", cmd: "format --check ./src"},
	}
	runRuleCases(t, cases, KindShellExec, "run_command")
}
