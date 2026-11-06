//go:build realstack

// Package realstack runs the gateway's published image shape against a real
// Django, its real database and its real search. The integration suite's fake
// Django only behaves the way its author expects, so what it cannot catch is
// a wrong belief about Django; these tests can.
//
// A clean Django takes minutes to migrate, so these run before a release and
// on a schedule (.github/workflows/real-stack.yml), not on every pull request.
package realstack

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Hosts of the store. `.localhost` is what Django's seed command accepts
// as non-production.
const (
	storeHost = "demostore.localhost"
	aliasHost = "alias.demostore.localhost"
	apiHost   = "api.demostore.localhost"
)

const (
	postgresImage = "postgres:18.6-alpine3.23"
	redisImage    = "redis:8-alpine"
	rabbitImage   = "rabbitmq:4.3.5-alpine"
	meiliImage    = "getmeili/meilisearch:v1.53.1"
	profileImage  = "busybox:1.37"
)

// startupDeadline bounds a service coming up; a management command gets longer.
const startupDeadline = 5 * time.Minute

//go:embed testdata/seed.py
var seedScript string

// seeded is what testdata/seed.py prints.
type seeded struct {
	ACPToken      string `json:"acpToken"`
	ClientID      string `json:"clientId"`
	LinkedToken   string `json:"linkedToken"`
	ForeignToken  string `json:"foreignToken"`
	BuyerEmail    string `json:"buyerEmail"`
	BuyerPassword string `json:"buyerPassword"`
}

// stack is the running system every test shares.
type stack struct {
	gatewayURL string
	profileURL string
	django     testcontainers.Container
	gateway    testcontainers.Container
	seeded     seeded
}

// printLogs shows what the services said, for a failed run in CI. Everything
// in this stack is throwaway, so nothing here is a secret worth hiding.
func (s *stack) printLogs(ctx context.Context) {
	for name, c := range map[string]testcontainers.Container{
		"django": s.django, "gateway": s.gateway,
	} {
		out, err := containerLogs(ctx, c)
		if err != nil {
			log.Printf("real stack: %s logs: %v", name, err)
			continue
		}
		log.Printf("real stack: %s logs:\n%s", name, tail(out))
	}
}

var running *stack

func TestMain(m *testing.M) {
	ctx := context.Background()
	s, cleanup, err := startStack(ctx)
	if err != nil {
		cleanup()
		log.Fatalf("real stack: %v", err)
	}
	running = s
	code := m.Run()
	if code != 0 {
		s.printLogs(ctx)
	}
	cleanup()
	os.Exit(code)
}

func requiredEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func randomHex() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func oidcKey() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "PRIVATE KEY", Bytes: der,
	})), nil
}

func startStack(ctx context.Context) (*stack, func(), error) {
	var containers []testcontainers.Container
	var removeNetwork func()
	cleanup := func() {
		for _, c := range containers {
			_ = testcontainers.TerminateContainer(c)
		}
		if removeNetwork != nil {
			removeNetwork()
		}
	}

	djangoImage, err := requiredEnv("REALSTACK_DJANGO_IMAGE")
	if err != nil {
		return nil, cleanup, err
	}
	gatewayImage, err := requiredEnv("REALSTACK_GATEWAY_IMAGE")
	if err != nil {
		return nil, cleanup, err
	}

	nw, err := network.New(ctx)
	if err != nil {
		return nil, cleanup, fmt.Errorf("network: %w", err)
	}
	removeNetwork = func() { _ = nw.Remove(ctx) }

	start := func(
		image, alias string, opts ...testcontainers.ContainerCustomizer,
	) (testcontainers.Container, error) {
		c, err := testcontainers.Run(ctx, image, append(opts,
			network.WithNetwork([]string{alias}, nw))...)
		if c != nil {
			containers = append(containers, c)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", alias, err)
		}
		return c, nil
	}

	meiliKey := randomHex()
	secret := randomHex()
	key, err := oidcKey()
	if err != nil {
		return nil, cleanup, err
	}

	// fsync off: a throwaway database, and migrating is most of the run.
	if _, err := start(postgresImage, "postgres",
		testcontainers.WithEnv(map[string]string{
			"POSTGRES_USER": "realstack", "POSTGRES_PASSWORD": "realstack",
			"POSTGRES_DB": "realstack",
		}),
		testcontainers.WithCmd("postgres", "-c", "fsync=off",
			"-c", "full_page_writes=off"),
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2)),
	); err != nil {
		return nil, cleanup, err
	}
	if _, err := start(redisImage, "redis",
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForLog("Ready to accept connections")),
	); err != nil {
		return nil, cleanup, err
	}
	if _, err := start(rabbitImage, "rabbitmq",
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForLog("Server startup complete")),
	); err != nil {
		return nil, cleanup, err
	}
	if _, err := start(meiliImage, "meilisearch",
		testcontainers.WithEnv(map[string]string{
			"MEILI_MASTER_KEY": meiliKey, "MEILI_ENV": "development",
		}),
		testcontainers.WithExposedPorts("7700/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForHTTP("/health").WithPort("7700/tcp")),
	); err != nil {
		return nil, cleanup, err
	}

	djangoEnv := map[string]string{
		"SYSTEM_ENV":                    "dev",
		"SECRET_KEY":                    randomHex(),
		"DB_HOST":                       "postgres",
		"DB_NAME":                       "realstack",
		"DB_USER":                       "realstack",
		"DB_PASSWORD":                   "realstack",
		"REDIS_HOST":                    "redis",
		"CELERY_BROKER_URL":             "amqp://guest:guest@rabbitmq:5672//",
		"MEILI_HOST":                    "meilisearch",
		"MEILI_MASTER_KEY":              meiliKey,
		"AGENT_GATEWAY_INTERNAL_SECRET": secret,
		"AGENT_GATEWAY_INTERNAL_URL":    "http://gateway:8080",
		"IDP_OIDC_PRIVATE_KEY":          key,
	}
	// A one-off management command, in a container of its own.
	manage := func(
		extra []testcontainers.ContainerCustomizer, args ...string,
	) (string, error) {
		name := args[0]
		cmd := append([]string{".venv/bin/python", "manage.py"}, args...)
		opts := append([]testcontainers.ContainerCustomizer{
			testcontainers.WithEnv(djangoEnv),
			testcontainers.WithCmd(cmd...),
			testcontainers.WithWaitStrategyAndDeadline(20*time.Minute,
				wait.ForExit()),
			network.WithNetwork([]string{"manage"}, nw),
		}, extra...)
		started := time.Now()
		c, err := testcontainers.Run(ctx, djangoImage, opts...)
		if c != nil {
			defer func() { _ = testcontainers.TerminateContainer(c) }()
		}
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		out, err := containerLogs(ctx, c)
		if err != nil {
			return "", fmt.Errorf("%s logs: %w", name, err)
		}
		state, err := c.State(ctx)
		if err != nil {
			return "", fmt.Errorf("%s state: %w", name, err)
		}
		log.Printf("real stack: %s took %s", name,
			time.Since(started).Round(time.Second))
		if state.ExitCode != 0 {
			return "", fmt.Errorf("%s exited %d:\n%s", name, state.ExitCode, tail(out))
		}
		return out, nil
	}

	if _, err := manage(nil, "migrate_schemas", "--noinput"); err != nil {
		return nil, cleanup, err
	}
	// The host the gateway resolves stores on: a platform tenant's.
	if _, err := manage(nil, "bootstrap_platform",
		"--domain", "localhost"); err != nil {
		return nil, cleanup, err
	}
	if _, err := manage(nil, "tenant_create",
		"--name", "Demostore", "--slug", "demostore", "--schema", "demostore",
		"--domain", storeHost, "--extra-domains", aliasHost, apiHost,
		"--owner-email", "owner@realstack.test", "--store-name", "Demostore",
	); err != nil {
		return nil, cleanup, err
	}
	// `pages` validates against a storefront, which this stack does not run.
	if _, err := manage(nil, "seed_demo_store", "--schema", "demostore",
		"--skip", "pages"); err != nil {
		return nil, cleanup, err
	}
	if _, err := manage(nil, "meilisearch_sync_all_indexes",
		"--all-tenants"); err != nil {
		return nil, cleanup, err
	}
	out, err := manage([]testcontainers.ContainerCustomizer{
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            strings.NewReader(seedScript),
			ContainerFilePath: "/tmp/seed.py",
			FileMode:          0o644,
		}),
	}, "shell", "-c", `exec(open("/tmp/seed.py").read())`)
	if err != nil {
		return nil, cleanup, err
	}
	data, err := parseSeeded(out)
	if err != nil {
		return nil, cleanup, err
	}

	// Static files are collected at start, as the deployment's init container
	// does: Django's pages need their manifest.
	django, err := start(djangoImage, "backend",
		testcontainers.WithEnv(djangoEnv),
		testcontainers.WithCmd("sh", "-c",
			".venv/bin/python manage.py collectstatic --noinput && "+
				"exec .venv/bin/gunicorn -c gunicorn.conf.py asgi:application"),
		testcontainers.WithExposedPorts("8000/tcp"),
		// Django redirects plain http to https unless the proxy says it was
		// https, as the ingress does.
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForHTTP("/api/v1/health/live").WithPort("8000/tcp").
				WithHeaders(map[string]string{"X-Forwarded-Proto": "https"})),
	)
	if err != nil {
		return nil, cleanup, err
	}

	profile, err := os.Open(filepath.Join(
		"..", "..", "testdata", "fixtures", "ucp",
		"runbook_platform_profile.json"))
	if err != nil {
		return nil, cleanup, err
	}
	defer func() { _ = profile.Close() }()
	if _, err := start(profileImage, "profile",
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader:            profile,
			ContainerFilePath: "/srv/profile.json",
			FileMode:          0o644,
		}),
		testcontainers.WithCmd("httpd", "-f", "-p", "8090", "-h", "/srv"),
		testcontainers.WithExposedPorts("8090/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForListeningPort("8090/tcp")),
	); err != nil {
		return nil, cleanup, err
	}

	gateway, err := start(gatewayImage, "gateway",
		testcontainers.WithEnv(map[string]string{
			// Development lets the gateway fetch a profile over plain http.
			"ENV":                       "development",
			"DJANGO_BASE_URL":           "http://backend:8000/api/v1",
			"DJANGO_PUBLIC_HOST":        "localhost",
			"REDIS_URL":                 "redis://redis:6379/1",
			"INTERNAL_EVENTS_SECRET":    secret,
			"ASSETS_HOST":               "assets.localhost",
			"STOREFRONT_DEFAULT_LOCALE": "el",
		}),
		testcontainers.WithExposedPorts("8080/tcp"),
		testcontainers.WithWaitStrategyAndDeadline(startupDeadline,
			wait.ForListeningPort("8080/tcp")),
	)
	if err != nil {
		return nil, cleanup, err
	}
	endpoint, err := gateway.PortEndpoint(ctx, "8080/tcp", "http")
	if err != nil {
		return nil, cleanup, err
	}
	return &stack{
		gatewayURL: endpoint,
		profileURL: "http://profile:8090/profile.json",
		django:     django,
		gateway:    gateway,
		seeded:     data,
	}, cleanup, nil
}

func containerLogs(
	ctx context.Context, c testcontainers.Container,
) (string, error) {
	r, err := c.Logs(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	var b bytes.Buffer
	if _, err := io.Copy(&b, r); err != nil {
		return "", err
	}
	return b.String(), nil
}

func tail(s string) string {
	const keep = 12000
	if len(s) > keep {
		return s[len(s)-keep:]
	}
	return s
}

func parseSeeded(out string) (seeded, error) {
	const marker = "REALSTACK "
	i := strings.LastIndex(out, marker)
	if i < 0 {
		return seeded{}, fmt.Errorf("seed printed no result:\n%s", tail(out))
	}
	line, _, _ := strings.Cut(out[i+len(marker):], "\n")
	var data seeded
	if err := json.Unmarshal([]byte(line), &data); err != nil {
		return seeded{}, fmt.Errorf("seed result: %w", err)
	}
	return data, nil
}
