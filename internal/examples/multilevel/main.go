package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"todo"
	"todo/ent"
	"todo/ent/migrate"

	"ariga.io/entcache"
	"entgo.io/contrib/entgql"
	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/playground"
	"github.com/alecthomas/kong"
	"github.com/go-redis/redis/v8"
	_ "github.com/mattn/go-sqlite3"
)

func main() {
	var cli struct {
		Addr      string `name:"address" default:":8081" help:"Address to listen on."`
		Cache     bool   `name:"cache" default:"true" help:"Enable context-level cache mode."`
		RedisAddr string `name:"redis" default:":6379" help:"Redis address"`
	}
	kong.Parse(&cli)
	db, err := sql.Open(dialect.SQLite, "file:ent?mode=memory&cache=shared&_fk=1")
	if err != nil {
		log.Fatal("opening database", err)
	}
	ctx := context.Background()
	// Run the migration without the debug information.
	if err := ent.NewClient(ent.Driver(db)).Schema.Create(ctx, migrate.WithGlobalUniqueID(true)); err != nil {
		log.Fatal("running schema migration", err)
	}
	drv := dialect.Debug(db)
	if cli.Cache {
		rdb := redis.NewClient(&redis.Options{
			Addr: cli.RedisAddr,
		})
		if err := rdb.Ping(ctx).Err(); err != nil {
			log.Fatal(err)
		}
		// In case of the cache cache is enabled, we wrap the driver with
		// a cache driver, and configures it to work in multi-level mode.
		drv = entcache.NewDriver(
			drv,
			entcache.TTL(time.Second*5),
			entcache.Levels(
				entcache.NewLRU(256),
				entcache.NewRedis(rdb),
			),
		)
	}
	client := ent.NewClient(ent.Driver(drv))
	srv := handler.NewDefaultServer(todo.NewSchema(client))
	srv.Use(entgql.Transactioner{TxOpener: client})
	http.Handle("/", playground.Handler("Todo", "/query"))
	http.Handle("/query", srv)
	http.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) {
		if cd, ok := drv.(*entcache.Driver); ok {
			stat := cd.Stats()
			fmt.Fprintf(w, "cache stats (gets: %d, hits: %d, errors: %d)\n", stat.Gets, stat.Hits, stat.Errors)
		} else {
			fmt.Fprintln(w, "cache mode is not enabled")
		}
	})
	log.Println("listening on", cli.Addr)
	if err := http.ListenAndServe(cli.Addr, nil); err != nil {
		log.Fatal("http server terminated", err)
	}
}
