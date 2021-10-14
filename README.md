# entcache

An experimental cache driver for [ent](https://github.com/ent/ent) with variety of storage options, such as:

1. A `context.Context`-based cache. Usually, attached to an HTTP request.

2. A driver level cache embedded in the `ent.Client`. Used to share cache entries on the process level.

4. A remote cache. For example, a Redis database that provides a persistence layer for storing and sharing cache
  entries between multiple processes.

4. A cache hierarchy, or multi-level cache allows structuring the cache in hierarchical way. For example, a 2-level cache
   that composed from an LRU-cache in the application memory, and a remote-level cache backed by a Redis database.

## Quick Introduction

First, `go get` the package using the following command.

```shell
go get ariga.io/entcache
```

After installing `entcache`, you can easily add it to your project with the snippet below:

```go
// Open the database connection.
db, err := sql.Open(dialect.SQLite, "file:ent?mode=memory&cache=shared&_fk=1")
if err != nil {
	log.Fatal("opening database", err)
}
// Decorates the sql.Driver with entcache.Driver.
drv := entcache.NewDriver(db)
// Create an ent.Client.
client := ent.NewClient(ent.Driver(drv))

// Tell the entcache.Driver to skip the caching layer
// when running the schema migration.
if client.Schema.Create(entcache.Skip(ctx)); err != nil {
	log.Fatal("running schema migration", err)
}

// Run queries.
if u, err := client.User.Get(ctx, id); err != nil {
	log.Fatal("querying user", err)
}
// The query below is cached.
if u, err := client.User.Get(ctx, id); err != nil {
	log.Fatal("querying user", err)
}
```

**However**, you need to choose the cache storage carefully before adding `entcache` to your project.
The section below covers the different approaches provided by this package.


## High Level Design

On a high level, `entcache.Driver` decorates the `Query` method of the given driver, and for each call, generates a cache
key (i.e. hash) from its arguments (i.e. statement and parameters). After the query is executed, the driver records the
raw values of the returned rows (`sql.Rows`), and stores them in the cache store with the generated cache key. This
means, that the recorded rows will be returned the next time the query is executed, if it was not evicted by the cache store.

The package provides a variety of options to configure the TTL of the cache entries, control the hash function, provide
custom and multi-level cache stores, evict and skip cache entries. See the full documentation in
[go.dev/entcache](https://pkg.go.dev/ariga.io/entcache).

### Caching Levels

`entcache` provides several builtin cache levels:

1. A `context.Context`-based cache. Usually, attached to a request and does not work with other cache levels.
It is used to eliminate duplicate queries that are executed by the same request.

2. A driver-level cache used by the `ent.Client`. An application usually creates a driver per database,
and therefore, we treat it as a process-level cache.

3. A remote cache. For example, a Redis database that provides a persistence layer for storing and sharing cache
  entries between multiple processes. A remote cache layer is resistant to application deployment changes or failures,
  and allows reducing the number of identical queries executed on the database by different process.

4. A cache hierarchy, or multi-level cache allows structuring the cache in hierarchical way. The hierarchy of cache
stores is mostly based on access speeds and cache sizes. For example, a 2-level cache that composed from an LRU-cache
in the application memory, and a remote-level cache backed by a Redis database.

#### Context Level Cache

The `ContextLevel` option configures the driver to work with a `context.Context` level cache. The context is usually
attached to a request (e.g. `*http.Request`) and is not available in multi-level mode. When this option is used as
a cache store, the attached `context.Context` carries an LRU cache (can be configured differently), and the driver
stores and searches entries in the LRU cache when queries are executed.

This option is ideal for applications that require strong consistency, but still want to avoid executing duplicate
database queries on the same request. For example, given the following GraphQL query:

```graphql
query($ids: [ID!]!) {
    nodes(ids: $ids) {
        ... on User {
            id
            name
            todos {
                id
                owner {
                    id
                    name
                }
            }
        }
    }
}
```

A naive solution for resolving the above query will execute, 1 for getting N users, another N queries for getting
the todos of each user, and a query for each todo item for getting its owner (read more about the
[_N+1 Problem_](https://entgo.io/docs/tutorial-todo-gql-field-collection/#problem)).

However, Ent provides a unique approach for resolving such queries(read more in
[Ent website](https://entgo.io/docs/tutorial-todo-gql-field-collection)) and therefore, only 3 queries will be executed
in this case. 1 for getting N users, 1 for getting the todo items of **all** users, and 1 query for getting the owners
of **all** todo items.

With `entcache`, the number of queries may be reduced to 2, as the first and last queries are identical (see
[code example](internal/examples/ctxlevel/main_test.go)).

![context-level-cache](https://github.com/ariga/entcache/blob/assets/internal/assets/ctxlevel.png)

##### Usage In GraphQL

In order to instantiate an `entcache.Driver` in a `ContextLevel` mode and use it in the generated `ent.Client` use the
following configuration.

```go
db, err := sql.Open(dialect.SQLite, "file:ent?mode=memory&cache=shared&_fk=1")
if err != nil {
	log.Fatal("opening database", err)
}
drv := entcache.NewDriver(db, entcache.ContextLevel())
client := ent.NewClient(ent.Driver(drv))
```

Then, when a GraphQL query hits the server, we wrap the request `context.Context` with an `entcache.NewContext`.

```go
srv.AroundResponses(func(ctx context.Context, next graphql.ResponseHandler) *graphql.Response {
	if op := graphql.GetOperationContext(ctx).Operation; op != nil && op.Operation == ast.Query {
		ctx = entcache.NewContext(ctx)
	}
	return next(ctx)
})
```

That's it! Your server is ready to use `entcache` with GraphQL, and a full server example exits in
[examples/ctxlevel](internal/examples/ctxlevel).

##### Middleware Example

An example of using the common middleware pattern in Go for wrapping the request `context.Context` with
an `entcache.NewContext` in case of `GET` requests.

```go
srv.Use(func(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			r = r.WithContext(entcache.NewContext(r.Context()))
		}
		next.ServeHTTP(w, r)
	})
})
```

#### Driver Level Cache

A driver-based level cached stores the cache entries on the `ent.Client`. An application usually creates a driver per
database (i.e. `sql.DB`), and therefore, we treat it as a process-level cache. The default cache storage for this option
is an LRU cache with no limit and no TTL for its entries, but can be configured differently.

![driver-level-cache](https://github.com/ariga/entcache/blob/assets/internal/assets/drvlevel.png)

##### Create a default cache driver, with no limit and no TTL.

```go
db, err := sql.Open(dialect.SQLite, "file:ent?mode=memory&cache=shared&_fk=1")
if err != nil {
	log.Fatal("opening database", err)
}
drv := entcache.NewDriver(db)
client := ent.NewClient(ent.Driver(drv))
```

##### Set the TTL to 1s.

```go
drv := entcache.NewDriver(drv, entcache.TTL(time.Second))
client := ent.NewClient(ent.Driver(drv))
```

##### Limit the cache to 128 entries and set the TTL to 1s.

```go
drv := entcache.NewDriver(
    drv,
    entcache.TTL(time.Second),
    entcache.Levels(entcache.NewLRU(128)),
)
client := ent.NewClient(ent.Driver(drv))
```

#### Remote Level Cache

A remote-based level cache is used to share cached entries between multiple processes. For example, a Redis database.
A remote cache layer is resistant to application deployment changes or failures, and allows reducing the number of
identical queries executed on the database by different processes. This option plays nicely the multi-level option below. 

#### Multi Level Cache

A cache hierarchy, or multi-level cache allows structuring the cache in hierarchical way. The hierarchy of cache
stores is mostly based on access speeds and cache sizes. For example, a 2-level cache that compounds from an LRU-cache
in the application memory, and a remote-level cache backed by a Redis database.

![context-level-cache](https://github.com/ariga/entcache/blob/assets/internal/assets/multilevel.png)

```go
rdb := redis.NewClient(&redis.Options{
    Addr: ":6379",
})
if err := rdb.Ping(ctx).Err(); err != nil {
    log.Fatal(err)
}
drv := entcache.NewDriver(
    drv,
    entcache.TTL(time.Second),
    entcache.Levels(
        entcache.NewLRU(256),
        entcache.NewRedis(rdb),
    ),
)
client := ent.NewClient(ent.Driver(drv))
```

### Future Work

There are a few features we are working on, and wish to work on, but need help from the community to design them
properly. If you are interested in one of the tasks or features below, do not hesitate to open an issue, or start a
discussion on GitHub or in [Ent Slack channel](https://entgo.io/docs/slack).

1. Add a Memcache implementation for a remote-level cache.
2. Support for smart eviction mechanism based on SQL parsing.
