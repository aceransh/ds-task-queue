package db

import (
	"database/sql"

	"log"
	"os"
	"time"

	_ "github.com/lib/pq"
)

var DB *sql.DB

func InitDB() {
	var err error

	connStr := os.Getenv("DB_DSN")
	if connStr == "" {
		connStr = "host=localhost port=5432 user=postgres password=db_password dbname=myDB sslmode=disable"
	}

	for i := 0; i <= 5; i++ {
		DB, err = sql.Open("postgres", connStr)
		if err != nil {
			log.Print("failed to open db: ", err)
			time.Sleep(time.Second * 2)
			continue
		}

		if err = DB.Ping(); err != nil {
			log.Print("Failed to ping db: ", err)
			time.Sleep(time.Second * 2)
			if i == 4 {
				log.Fatal("completely failed to connect to db after 5 tries")
			}
			continue
		}

		log.Println("Successfully connected to the database!")
		break
	}
}
