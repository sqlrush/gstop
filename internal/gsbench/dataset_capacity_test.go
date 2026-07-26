package gsbench

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type fakeDatasetCapacityProvider struct {
	facts []StorageCapacityFact
	err   error
}

func (p fakeDatasetCapacityProvider) CapacityFacts(context.Context, Environment) ([]StorageCapacityFact, error) {
	return append([]StorageCapacityFact(nil), p.facts...), p.err
}

func TestDistributedCapacityRequiresEveryPrimaryDN(t *testing.T) {
	env := Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
		Nodes: []Node{
			{Name: "dn_1", Role: NodeRoleDNPrimary},
			{Name: "dn_2", Role: NodeRoleDNPrimary},
			{Name: "cn_1", Role: NodeRoleCN},
		},
	}
	_, _, err := resolveDatasetCapacity(
		context.Background(),
		fakeDatasetCapacityProvider{facts: []StorageCapacityFact{{
			NodeName: "dn_1", TotalBytes: 100 << 30, FreeBytes: 80 << 30,
		}}},
		env,
		false,
		5<<30,
		20,
	)
	if err == nil || !strings.Contains(err.Error(), "dn_2") {
		t.Fatalf("missing primary DN was accepted: %v", err)
	}
}

func TestDistributedCapacityRejectsAnyPrimaryDNBelowItsReserve(t *testing.T) {
	env := Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
		Nodes: []Node{
			{Name: "dn_1", Role: NodeRoleDNPrimary},
			{Name: "dn_2", Role: NodeRoleDNPrimary},
		},
	}
	_, _, err := resolveDatasetCapacity(
		context.Background(),
		fakeDatasetCapacityProvider{facts: []StorageCapacityFact{
			{NodeName: "dn_1", TotalBytes: 100 << 30, FreeBytes: 80 << 30},
			{NodeName: "dn_2", TotalBytes: 100 << 30, FreeBytes: 10 << 30},
		}},
		env,
		false,
		5<<30,
		20,
	)
	if err == nil || !strings.Contains(err.Error(), "dn_2") {
		t.Fatalf("unsafe primary DN was accepted: %v", err)
	}
}

func TestDistributedCapacityRequiresTargetShareToFitEveryPrimaryDN(t *testing.T) {
	env := Environment{
		Product:  ProductGaussDB,
		Topology: TopologyDistributed,
		Nodes: []Node{
			{Name: "dn_1", Role: NodeRoleDNPrimary},
			{Name: "dn_2", Role: NodeRoleDNPrimary},
		},
	}
	_, _, err := resolveDatasetCapacity(
		context.Background(),
		fakeDatasetCapacityProvider{facts: []StorageCapacityFact{
			{NodeName: "dn_1", TotalBytes: 100 << 30, FreeBytes: 45 << 30},
			{NodeName: "dn_2", TotalBytes: 100 << 30, FreeBytes: 90 << 30},
		}},
		env,
		false,
		60<<30,
		20,
	)
	if err == nil || !strings.Contains(err.Error(), "target_share") ||
		!strings.Contains(err.Error(), "dn_1") {
		t.Fatalf("small primary DN accepted: %v", err)
	}
}

func TestUnavailableCapacityRefusesRealInitButAllowsExplicitDryRun(t *testing.T) {
	provider := fakeDatasetCapacityProvider{err: errors.New("provider unavailable")}
	env := testDatasetEnvironment()
	if _, _, err := resolveDatasetCapacity(
		context.Background(), provider, env, false, 5<<30, 20,
	); err == nil {
		t.Fatal("real init accepted unavailable target storage")
	}
	capacity, status, err := resolveDatasetCapacity(
		context.Background(), provider, env, true, 5<<30, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if status.Source != "unavailable" || status.Error == "" {
		t.Fatalf("dry-run status=%+v", status)
	}
	if capacity.FreeBytes < 5<<30 {
		t.Fatalf("dry-run planning capacity=%+v", capacity)
	}
}

type fakeCapacityDatabase struct {
	query    string
	scanArgs []any
	scan     func(dest ...any) error
}

func (d *fakeCapacityDatabase) Query(_ context.Context, query string, _ ...any) (journalRows, error) {
	d.query = query
	return nil, errors.New("unexpected query")
}

func (d *fakeCapacityDatabase) Scan(
	_ context.Context,
	query string,
	args []any,
	dest ...any,
) error {
	d.query = query
	d.scanArgs = append([]any(nil), args...)
	return d.scan(dest...)
}

func (d *fakeCapacityDatabase) Exec(context.Context, string, ...any) (sql.Result, error) {
	return nil, errors.New("unexpected exec")
}

func TestTablespaceCapacityProviderUsesFiniteDefaultTablespaceQuota(t *testing.T) {
	db := &fakeCapacityDatabase{scan: func(dest ...any) error {
		*(dest[0].(*string)) = "pg_default"
		*(dest[1].(*sql.NullString)) = sql.NullString{
			String: "104857600 K", Valid: true,
		}
		*(dest[2].(*int64)) = 25 << 30
		return nil
	}}
	facts, err := (tablespaceQuotaCapacityProvider{db: db}).CapacityFacts(
		context.Background(), testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(db.query, "pg_catalog.pg_tablespace") ||
		!strings.Contains(db.query, "spcmaxsize") ||
		!strings.Contains(db.query, "pg_tablespace_size") {
		t.Fatalf("capacity query does not use documented tablespace quota:\n%s", db.query)
	}
	want := []StorageCapacityFact{{
		NodeName: "pg_default", TotalBytes: 100 << 30, FreeBytes: 75 << 30,
		Source: "tablespace_quota",
	}}
	if !reflect.DeepEqual(facts, want) {
		t.Fatalf("facts=%+v want=%+v", facts, want)
	}
}

func TestTablespaceCapacityProviderUsesCurrentDatabaseTablespaceWhenSettingIsEmpty(t *testing.T) {
	db := &fakeCapacityDatabase{scan: func(dest ...any) error {
		*(dest[0].(*string)) = "custom_database_default"
		*(dest[1].(*sql.NullString)) = sql.NullString{
			String: "10 G", Valid: true,
		}
		*(dest[2].(*int64)) = 1 << 30
		return nil
	}}
	facts, err := (tablespaceQuotaCapacityProvider{db: db}).CapacityFacts(
		context.Background(), testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"pg_catalog.pg_database d",
		"d.datname=current_database()",
		"THEN d.dattablespace",
		"JOIN pg_catalog.pg_tablespace t ON t.oid=selected.oid",
	} {
		if !strings.Contains(db.query, required) {
			t.Fatalf("empty-setting query does not resolve the database default via %q:\n%s", required, db.query)
		}
	}
	if len(facts) != 1 || facts[0].NodeName != "custom_database_default" {
		t.Fatalf("facts=%+v", facts)
	}
}

func TestTablespaceCapacityProviderUsesExplicitSessionTablespace(t *testing.T) {
	db := &fakeCapacityDatabase{scan: func(dest ...any) error {
		*(dest[0].(*string)) = "explicit_session_tablespace"
		*(dest[1].(*sql.NullString)) = sql.NullString{
			String: "10 G", Valid: true,
		}
		*(dest[2].(*int64)) = 1 << 30
		return nil
	}}
	facts, err := (tablespaceQuotaCapacityProvider{db: db}).CapacityFacts(
		context.Background(), testDatasetEnvironment(),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"WHEN NULLIF(trim(current_setting('default_tablespace')),'') IS NULL",
		"configured.spcname=NULLIF(trim(current_setting('default_tablespace')),'')",
	} {
		if !strings.Contains(db.query, required) {
			t.Fatalf("explicit-setting query does not resolve the named tablespace via %q:\n%s", required, db.query)
		}
	}
	if len(facts) != 1 || facts[0].NodeName != "explicit_session_tablespace" {
		t.Fatalf("facts=%+v", facts)
	}
}

func TestTablespaceProviderRejectsUnlimitedQuotaWithActionableError(t *testing.T) {
	db := &fakeCapacityDatabase{scan: func(dest ...any) error {
		*(dest[0].(*string)) = "pg_default"
		*(dest[1].(*sql.NullString)) = sql.NullString{}
		*(dest[2].(*int64)) = 25 << 30
		return nil
	}}
	_, err := (tablespaceQuotaCapacityProvider{db: db}).CapacityFacts(
		context.Background(), testDatasetEnvironment(),
	)
	if err == nil ||
		!strings.Contains(err.Error(), "provider wiring is not implemented in this build") ||
		strings.Contains(err.Error(), "configure external") {
		t.Fatalf("err=%v", err)
	}
}

func TestParseTablespaceQuotaUsesDocumentedByteAndUnitFormats(t *testing.T) {
	for value, want := range map[string]int64{
		"107374182400": 100 << 30,
		"104857600 K":  100 << 30,
		"102400 M":     100 << 30,
		"100 G":        100 << 30,
	} {
		got, err := parseTablespaceQuota(value)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if got != want {
			t.Fatalf("%q parsed=%d want=%d", value, got, want)
		}
	}
}

func TestLocalDataDirectoryProviderRejectsRemoteEndpointBeforeQuery(t *testing.T) {
	db := &fakeCapacityDatabase{scan: func(...any) error {
		return errors.New("must not query remote data_directory")
	}}
	_, err := (localDataDirectoryCapacityProvider{
		db: db, host: "db.example.com",
	}).CapacityFacts(context.Background(), testDatasetEnvironment())
	if err == nil || !strings.Contains(err.Error(), "provably local") {
		t.Fatalf("err=%v", err)
	}
	if db.query != "" {
		t.Fatalf("remote endpoint queried: %s", db.query)
	}
}

func TestLocalDataDirectoryProviderNeverFallsBackFromMissingServerPath(t *testing.T) {
	db := &fakeCapacityDatabase{scan: func(dest ...any) error {
		*(dest[0].(*string)) = "/server/path/not/on/client"
		return nil
	}}
	called := false
	_, err := (localDataDirectoryCapacityProvider{
		db:   db,
		host: "127.0.0.1",
		stat: func(path string) (int64, int64, error) {
			called = true
			if path != "/server/path/not/on/client" {
				t.Fatalf("stat path=%q", path)
			}
			return 0, 0, errors.New("path does not exist")
		},
	}).CapacityFacts(context.Background(), testDatasetEnvironment())
	if err == nil || !called ||
		!strings.Contains(err.Error(), "/server/path/not/on/client") {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestDistributedProviderSelectionRequiresBuildTimeWiring(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.CapacityProvider = "auto"
	cfg.Data.PhysicalSizeProvider = "auto"
	env := distributedFixture()
	_, err := selectDatasetCapacityProvider(
		cfg, env, &fakeCapacityDatabase{}, DatasetExternalProviders{},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "provider wiring is not implemented in this build") ||
		strings.Contains(err.Error(), "configure external") {
		t.Fatalf("capacity selection err=%v", err)
	}
	_, err = selectDatasetPhysicalProvider(
		cfg, env, &fakeCapacityDatabase{}, DatasetExternalProviders{},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "provider wiring is not implemented in this build") ||
		strings.Contains(err.Error(), "configure external") {
		t.Fatalf("physical selection err=%v", err)
	}
}

func TestDistributedCapacityProviderCanBeInjectedInternally(t *testing.T) {
	cfg := datasetConfig("quick", 5)
	cfg.Data.CapacityProvider = "auto"
	provider := fakeDatasetCapacityProvider{facts: []StorageCapacityFact{{
		NodeName: "dn_1", TotalBytes: 100 << 30, FreeBytes: 80 << 30,
	}}}
	selected, err := selectDatasetCapacityProvider(
		cfg,
		Environment{
			Product:  ProductGaussDB,
			Topology: TopologyDistributed,
			Nodes:    []Node{{Name: "dn_1", Role: NodeRoleDNPrimary}},
		},
		&fakeCapacityDatabase{},
		DatasetExternalProviders{Capacity: provider},
	)
	if err != nil {
		t.Fatal(err)
	}
	facts, err := selected.CapacityFacts(context.Background(), distributedFixture())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(facts, provider.facts) {
		t.Fatalf("facts=%+v want=%+v", facts, provider.facts)
	}
}
