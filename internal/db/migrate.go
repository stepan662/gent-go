package db

import (
	"database/sql"
	"errors"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	pgmigrate "github.com/golang-migrate/migrate/v4/database/postgres"
	sqlite3migrate "github.com/golang-migrate/migrate/v4/database/sqlite3"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

func runMigrations(sqldb *sql.DB, dialect string) error {
	src, err := iofs.New(sqlMigrations, "migrations")
	if err != nil {
		return err
	}

	var driver database.Driver
	switch dialect {
	case "sqlite":
		driver, err = sqlite3migrate.WithInstance(sqldb, &sqlite3migrate.Config{})
	case "postgres":
		driver, err = pgmigrate.WithInstance(sqldb, &pgmigrate.Config{})
	}
	if err != nil {
		return err
	}

	m, err := migrate.NewWithInstance("iofs", src, dialect, driver)
	if err != nil {
		return err
	}

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}
