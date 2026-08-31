package sqlitetopostgres

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestBuildTableLoadPlanDefersNullableCycle(t *testing.T) {
	sources := map[string]*sourceTable{
		"owners":  {Name: "owners"},
		"budgets": {Name: "budgets"},
	}
	targets := map[string]*targetTable{
		"owners": {
			Name: "owners",
			ColumnByName: map[string]*targetColumn{
				"budget_id": {Name: "budget_id", Nullable: true},
			},
			ForeignKeys: []foreignKey{{ChildTable: "owners", ParentTable: "budgets", ChildColumns: []string{"budget_id"}}},
		},
		"budgets": {
			Name: "budgets",
			ColumnByName: map[string]*targetColumn{
				"owner_id": {Name: "owner_id", Nullable: true},
			},
			ForeignKeys: []foreignKey{{ChildTable: "budgets", ParentTable: "owners", ChildColumns: []string{"owner_id"}}},
		},
	}
	plan, err := buildTableLoadPlan(sources, targets)
	require.NoError(t, err)
	require.Len(t, plan.Order, 2)
	_, ownerDeferred := plan.DeferredColumns["owners"]["budget_id"]
	_, budgetDeferred := plan.DeferredColumns["budgets"]["owner_id"]
	require.True(t, ownerDeferred)
	require.True(t, budgetDeferred)
}

func TestBuildTableLoadPlanRejectsNonNullableCycle(t *testing.T) {
	sources := map[string]*sourceTable{"nodes": {Name: "nodes"}}
	targets := map[string]*targetTable{
		"nodes": {
			Name: "nodes",
			ColumnByName: map[string]*targetColumn{
				"parent_id": {Name: "parent_id", Nullable: false},
			},
			ForeignKeys: []foreignKey{{ChildTable: "nodes", ParentTable: "nodes", ChildColumns: []string{"parent_id"}}},
		},
	}
	_, err := buildTableLoadPlan(sources, targets)
	require.ErrorContains(t, err, "requires nullable column")
}

func TestValidateTableContractRejectsLossyDateTarget(t *testing.T) {
	source := &sourceTable{
		Store: "logs",
		Name:  "events",
		Columns: []sourceColumn{{
			Name: "created_at", DeclaredType: "DATETIME", NotNull: true,
		}},
	}
	target := &targetTable{
		Name: "events",
		ColumnByName: map[string]*targetColumn{
			"created_at": {Name: "created_at", DataType: "date", Nullable: false},
		},
	}
	err := validateTableContract(source, target)
	require.ErrorContains(t, err, "column type mismatch")
}

func TestExtractSQLiteChecksNormalizesPostgresCasts(t *testing.T) {
	checks := extractSQLiteChecks(`CREATE TABLE logs (provider TEXT CHECK (provider <> 'blocked'), count INTEGER, CHECK (count >= 0))`)
	require.Len(t, checks, 2)
	require.True(t, equalChecks(checks, []string{`((provider <> 'blocked'::text))`, `(count >= 0)`}))
}

func TestDiscoverSourceSequenceHighWaterAfterDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	db := openWritableSQLite(t, path)
	execSQLiteStatements(t, db,
		`CREATE TABLE records (id INTEGER PRIMARY KEY AUTOINCREMENT, value TEXT)`,
		`INSERT INTO records(id, value) VALUES (41, 'retained'), (100, 'deleted')`,
		`DELETE FROM records WHERE id = 100`,
	)
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	tables, err := discoverSourceTables(context.Background(), tx, "config")
	require.NoError(t, err)
	require.NoError(t, discoverSourceSequenceHighWater(context.Background(), tx, tables))
	require.Equal(t, int64(100), *tables["records"].AutoIncrementHighWater)
	require.NoError(t, tx.Rollback())
	require.NoError(t, db.Close())
}
