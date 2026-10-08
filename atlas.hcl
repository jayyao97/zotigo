env "catalog" {
  src = "file://core/catalogschema/schema.sql"
  dev = "sqlite://file?mode=memory&_fk=1"
  migration {
    dir = "file://core/catalogschema/migrations"
    format = golang-migrate
  }
}
