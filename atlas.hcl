env "catalog" {
  src = "file://core/workspace/schema.sql"
  dev = "sqlite://file?mode=memory&_fk=1"
  migration {
    dir = "file://core/workspace/migrations"
    format = golang-migrate
  }
}
