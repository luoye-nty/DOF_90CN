package mysql

import (
	"context"
	"errors"
	"longheng.io/server/internal/modules/dnf/repository"
	"testing"
)

func TestSchemaComponentStartCreatesSchemaWhenEnabled(t *testing.T) {
	exec := &fakeSchemaExec{}
	options := SchemaOptions{
		AutoCreate:      true,
		CreateDatabases: true,
		DatabasePlan: repository.DatabasePlan{
			ShardID:        "9999",
			WriteDatabases: []string{"dnf_s9999_w1"},
			ReadDatabases:  []string{"dnf_s9999_r1"},
		},
	}
	component := NewSchemaComponent(exec, options)
	if component.Name() != "dnf-repository-schema" {
		t.Fatalf("unexpected component name: %s", component.Name())
	}
	if err := component.Preflight(context.Background()); err != nil {
		t.Fatalf("preflight: %v", err)
	}
	if err := component.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 这里的重点是"组件把整份 schema 原样跑了一遍"（建库语句 + 增量迁移）。
	// 期望条数由生成器推导，避免每加一列就在这个无关测试里改一个魔数；
	// 精确条数与迁移顺序、内容由 schema_test.go 断言（那里才是防漏语句的地方）。
	wantStatements, err := MySQLSchema(options)
	if err != nil {
		t.Fatalf("MySQLSchema() error = %v", err)
	}
	migrations, err := mysqlSchemaMigrations(options)
	if err != nil {
		t.Fatalf("mysqlSchemaMigrations() error = %v", err)
	}
	if len(wantStatements) == 0 || len(migrations) == 0 {
		t.Fatalf("schema generator returned nothing: tables=%d migrations=%d", len(wantStatements), len(migrations))
	}
	want := len(wantStatements) + len(migrations)
	if len(exec.statements) != want {
		t.Fatalf("executed statements = %d, want %d", len(exec.statements), want)
	}
	snapshot := component.Snapshot()
	if !snapshot.Started || !snapshot.AutoCreate || snapshot.ShardID != "9999" {
		t.Fatalf("unexpected snapshot: %+v", snapshot)
	}
}

func TestSchemaComponentNoopsWhenDisabled(t *testing.T) {
	exec := &fakeSchemaExec{}
	component := NewSchemaComponent(exec, SchemaOptions{
		DatabasePlan: repository.DatabasePlan{WriteDatabases: []string{"dnf_s9999_w1"}},
	})
	if err := component.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if len(exec.statements) != 0 {
		t.Fatalf("disabled component should not execute schema: %v", exec.statements)
	}
}

func TestSchemaComponentRejectsMissingExecutorWhenEnabled(t *testing.T) {
	component := NewSchemaComponent(nil, SchemaOptions{
		AutoCreate:   true,
		DatabasePlan: repository.DatabasePlan{WriteDatabases: []string{"dnf_s9999_w1"}},
	})
	if err := component.Preflight(context.Background()); !errors.Is(err, ErrSchemaExecutorRequired) {
		t.Fatalf("Preflight() error = %v, want ErrSchemaExecutorRequired", err)
	}
	if err := component.Start(context.Background()); !errors.Is(err, ErrSchemaExecutorRequired) {
		t.Fatalf("Start() error = %v, want ErrSchemaExecutorRequired", err)
	}
	if snapshot := component.Snapshot(); snapshot.Started || snapshot.LastError == "" {
		t.Fatalf("expected failed snapshot, got %+v", snapshot)
	}
}
