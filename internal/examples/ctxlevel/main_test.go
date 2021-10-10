package main_test

import (
	"context"
	"testing"

	"todo"
	"todo/ent"
	"todo/ent/migrate"
	"todo/ent/user"

	"ariga.io/entcache"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	gqlclient "github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	_ "github.com/mattn/go-sqlite3"
)

func TestContextLevel(t *testing.T) {
	db, err := sql.Open(dialect.SQLite, "file:ent?mode=memory&cache=shared&_fk=1")
	if err != nil {
		t.Fatal("opening database", err)
	}
	ctx := context.Background()
	// Run the migration without the debug information.
	if err := ent.NewClient(ent.Driver(db)).Schema.Create(ctx, migrate.WithGlobalUniqueID(true)); err != nil {
		t.Fatal("running schema migration", err)
	}
	// Wraps the driver with a query counter.
	q := &queryCount{Driver: db}
	// Wrap the driver with a cache driver, and configure
	// it to work in a context-level mode.
	drv := entcache.NewDriver(q, entcache.ContextLevel())
	client := ent.NewClient(ent.Driver(drv))

	parent := client.Todo.Create().SetText("parent").SaveX(ctx)
	children := client.Todo.CreateBulk(
		client.Todo.Create().SetText("child-1").SetParent(parent),
		client.Todo.Create().SetText("child-2").SetParent(parent),
		client.Todo.Create().SetText("child-3").SetParent(parent),
	).SaveX(ctx)
	client.User.CreateBulk(
		client.User.Create().SetName("a8m").AddTodos(parent, children[1]),
		client.User.Create().SetName("nati").AddTodos(children[2:]...),
	).ExecX(ctx)
	ids := client.User.Query().IDsX(ctx)

	t.Run("WithoutCache", func(t *testing.T) {
		q.expectCount(t, 3, func() {
			client.User.Query().
				Where(user.IDIn(ids...)).
				WithTodos(func(q *ent.TodoQuery) {
					q.WithOwner()
				}).
				AllX(ctx)
		})
	})

	t.Run("WithCache", func(t *testing.T) {
		ctx := entcache.NewContext(ctx)
		q.expectCount(t, 2, func() {
			client.User.Query().
				Where(user.IDIn(ids...)).
				WithTodos(func(q *ent.TodoQuery) {
					q.WithOwner()
				}).
				AllX(ctx)
		})
	})

	// Demonstrate the usage with GraphQL.
	t.Run("GQL", func(t *testing.T) {
		const query = `query($ids: [ID!]!) {
		  nodes(ids: $ids) {
		    ... on User {
		      id
		      todos {
		        id
		        owner {
		          id
		          name
		        }
		      }
		    }
		  }
		}`
		// Load the ent_types table on the first Node query.
		if _, err := client.Noders(ctx, ids); err != nil {
			t.Fatal(err)
		}
		t.Run("WithoutCache", func(t *testing.T) {
			var (
				rsp interface{}
				srv = handler.NewDefaultServer(todo.NewSchema(client))
				gql = gqlclient.New(srv)
			)
			q.expectCount(t, 3, func() {
				if err := gql.Post(query, &rsp, gqlclient.Var("ids", ids)); err != nil {
					t.Fatal(err)
				}
			})
		})
		t.Run("WithCache", func(t *testing.T) {
			var (
				rsp interface{}
				srv = handler.NewDefaultServer(todo.NewSchema(client))
				gql = gqlclient.New(srv)
			)
			srv.AroundResponses(func(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
				return next(entcache.NewContext(ctx))
			})
			q.expectCount(t, 2, func() {
				if err := gql.Post(query, &rsp, gqlclient.Var("ids", ids)); err != nil {
					t.Fatal(err)
				}
			})
		})
	})
}

type queryCount struct {
	n int
	dialect.Driver
}

func (q *queryCount) Query(ctx context.Context, query string, args, v interface{}) error {
	q.n++
	return q.Driver.Query(ctx, query, args, v)
}

// expectCount expects the given function to execute "n" queries.
func (q *queryCount) expectCount(t *testing.T, n int, fn func()) {
	q.n = 0
	fn()
	if q.n != n {
		t.Errorf("expect client to execute %d queries, got: %d", n, q.n)
	}
}
