package main

import (
	"context"
	"log"
	"net/http"

	"todo"
	"todo/ent"
	"todo/ent/migrate"

	"ariga.io/entcache"
	"entgo.io/contrib/entgql"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/alecthomas/kong"
	_ "github.com/mattn/go-sqlite3"
	"github.com/vektah/gqlparser/v2/ast"
)

func main() {
	var cli struct {
		Addr  string `name:"address" default:":8081" help:"Address to listen on."`
		Cache bool   `name:"cache" default:"true" help:"Enable context-level cache mode."`
	}
	kong.Parse(&cli)
	db, err := sql.Open(dialect.SQLite, "file:ent?mode=memory&cache=shared&_fk=1")
	if err != nil {
		log.Fatal("opening database", err)
	}
	// Run the migration without the debug information.
	if err := ent.NewClient(ent.Driver(db)).Schema.Create(
		context.Background(),
		migrate.WithGlobalUniqueID(true),
	); err != nil {
		log.Fatal("running schema migration", err)
	}
	drv := dialect.Debug(db)
	if cli.Cache {
		// In case of the context/request level cache is enabled, we wrap the
		// driver with a cache driver, and configures it to work in this mode.
		drv = entcache.NewDriver(drv, entcache.ContextLevel())
	}
	client := ent.NewClient(ent.Driver(drv))
	srv := handler.NewDefaultServer(todo.NewSchema(client))
	srv.Use(entgql.Transactioner{TxOpener: client})
	if cli.Cache {
		// In case of the context/request level cache is enabled, we add a middleware
		// that wraps the context of GraphQL queries with cache context.
		srv.AroundResponses(func(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
			if op := graphql.GetOperationContext(ctx).Operation; op != nil && op.Operation == ast.Query {
				ctx = entcache.NewContext(ctx)
			}
			return next(ctx)
		})
	}
	http.Handle("/", playground.Handler("Todo", "/query"))
	http.Handle("/query", srv)
	log.Println("listening on", cli.Addr)
	if err := http.ListenAndServe(cli.Addr, nil); err != nil {
		log.Fatal("http server terminated", err)
	}
}
