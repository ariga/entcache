### Run Redis Container

```shell
docker run --rm -p 6379:6379 redis
```

### Run The App

```shell
go run main.go
```

Open [localhost:8081](http://localhost:8081/) and execute GraphQL queries.

### Get Cache Stats

```shell
curl :8081/stats

# cache stats (gets: 61, hits: 44, errors: 0)
```
