package compose

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func cleanComposeEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"COMPOSE_FILE", "COMPOSE_PATH_SEPARATOR", "COMPOSE_PROJECT_NAME", "COMPOSE_PROFILES", "COMPOSE_DISABLE_ENV_FILE"} {
		before, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, before)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func writeFixture(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestComposeDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		explicit, fromEnv, parent bool
		want                      string
	}{
		{name: "automatic override", want: "override"},
		{name: "explicit file excludes automatic override", explicit: true, want: "base"},
		{name: "COMPOSE_FILE", fromEnv: true, want: "override"},
		{name: "explicit file beats COMPOSE_FILE", explicit: true, fromEnv: true, want: "base"},
		{name: "search parent directories", parent: true, want: "override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanComposeEnv(t)
			dir := t.TempDir()
			base := writeFixture(t, dir, "compose.yaml", "name: discovery\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    environment:\n      MODE: base\n")
			override := writeFixture(t, dir, "compose.override.yaml", "services:\n  web:\n    environment:\n      MODE: override\n")
			cwd := dir
			if tc.parent {
				cwd = filepath.Join(dir, "child")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(cwd)
			var opts Options
			if tc.fromEnv {
				t.Setenv("COMPOSE_FILE", base+string(os.PathListSeparator)+override)
			}
			if tc.explicit {
				opts.Files = []string{base}
			}
			project, err := Load(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			actual := project.Services["web"].Environment["MODE"]
			if actual == nil || *actual != tc.want {
				t.Fatalf("wanted MODE=%s, got %v", tc.want, actual)
			}
		})
	}
}

func TestComposeProfiles(t *testing.T) {
	for _, tc := range []struct {
		name, env  string
		flags      []string
		fromDotEnv bool
		want       []string
	}{
		{name: "shell profiles", env: "extra", want: []string{"base", "optional"}},
		{name: "dotenv profiles", env: "extra", fromDotEnv: true, want: []string{"base", "optional"}},
		{name: "flag beats environment", env: "extra", flags: []string{"different"}, want: []string{"base", "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanComposeEnv(t)
			dir := t.TempDir()
			t.Chdir(dir)
			writeFixture(t, dir, "compose.yaml", "name: profiles\nservices:\n  base:\n    image: docker.io/library/nginx:1.27\n  optional:\n    image: docker.io/library/nginx:1.27\n    profiles: [extra]\n  other:\n    image: docker.io/library/nginx:1.27\n    profiles: [different]\n")
			if tc.fromDotEnv {
				writeFixture(t, dir, ".env", "COMPOSE_PROFILES="+tc.env+"\n")
			} else {
				t.Setenv("COMPOSE_PROFILES", tc.env)
			}
			p, err := Load(context.Background(), Options{Profiles: tc.flags})
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Services) != len(tc.want) {
				t.Fatalf("services: %v", p.ServiceNames())
			}
			for _, name := range tc.want {
				if _, ok := p.Services[name]; !ok {
					t.Fatalf("missing %s in %v", name, p.ServiceNames())
				}
			}
		})
	}
}

func TestDotEnvCanSelectComposeFile(t *testing.T) {
	cleanComposeEnv(t)
	dir := t.TempDir()
	t.Chdir(dir)
	projectDir := filepath.Join(dir, "project")
	if err := os.Mkdir(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeFixture(t, dir, ".env", "COMPOSE_FILE=project/custom.yaml\nREVIEW_FROM_ENV=working-directory\n")
	writeFixture(t, projectDir, ".env", "REVIEW_FROM_ENV=project-directory\nREVIEW_PROJECT_ENV=present\n")
	writeFixture(t, projectDir, "custom.yaml", "name: env\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    environment:\n      SOURCE: ${REVIEW_FROM_ENV}\n      PROJECT: ${REVIEW_PROJECT_ENV}\n")
	p, err := Load(context.Background(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	env := p.Services["web"].Environment
	if env["SOURCE"] == nil || *env["SOURCE"] != "working-directory" || env["PROJECT"] == nil || *env["PROJECT"] != "present" {
		t.Fatalf("wrong dotenv precedence: %#v", env)
	}
}

func TestHTTPPortSurvivesComposeOverridesAndInterpolation(t *testing.T) {
	cleanComposeEnv(t)
	dir := t.TempDir()
	base := writeFixture(t, dir, "compose.yaml", "name: override-test\nservices:\n  web:\n    image: docker.io/library/nginx:1.27\n    http-port: 80\n    ports: [{target: 80}]\n")
	override := writeFixture(t, dir, "override.yaml", "services:\n  web:\n    http-port: ${APP_PORT}\n    ports: [{target: 8080}]\n")
	t.Setenv("APP_PORT", "8080")
	project, err := Load(context.Background(), Options{Files: []string{base, override}})
	if err != nil {
		t.Fatal(err)
	}
	if got := project.Services["web"].Extensions[HTTPPortExtension]; got != "8080" {
		t.Fatalf("override/interpolation lost http-port: %#v", got)
	}

	disable := writeFixture(t, dir, "internal.yaml", "services:\n  web:\n    http-port: null\n")
	project, err = Load(context.Background(), Options{Files: []string{base, override, disable}})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := project.Services["web"].Extensions[HTTPPortExtension]; ok {
		t.Fatalf("null override did not remove http-port: %#v", got)
	}
}

func TestRewriteHTTPPortLeavesOtherFilesUntouched(t *testing.T) {
	content := []byte("# keep this comment\nservices:\n  web:   {image: docker.io/library/nginx:1.27}\n")
	rewritten, isNull, err := rewriteHTTPPort(content)
	if err != nil {
		t.Fatal(err)
	}
	if string(rewritten) != string(content) || len(isNull) != 0 {
		t.Fatalf("file without http-port was changed:\n%s", rewritten)
	}
}
