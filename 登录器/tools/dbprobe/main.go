package main

import (
	"database/sql"
	"fmt"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	dsn := "root:aa123456@tcp(127.0.0.1:13306)/dnf_local?charset=utf8mb4"
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		fmt.Println("OPEN_ERR:", err)
		return
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		fmt.Println("PING_ERR:", err)
		return
	}
	fmt.Println("PING_OK")

	rows, err := db.Query("SHOW CREATE TABLE dnf_login_accounts")
	if err != nil {
		fmt.Println("SHOW_ERR:", err)
		return
	}
	for rows.Next() {
		var name, ddl string
		rows.Scan(&name, &ddl)
		fmt.Println("DDL_START")
		fmt.Println(ddl)
		fmt.Println("DDL_END")
	}
	rows.Close()

	rows2, err := db.Query("SELECT account_id, username, password_hash, state, created_at, updated_at, last_login_at FROM dnf_login_accounts ORDER BY created_at DESC LIMIT 3")
	if err != nil {
		fmt.Println("SEL_ERR:", err)
		return
	}
	for rows2.Next() {
		var aid, uname, phash, state sql.NullString
		var ca, ua, lla sql.NullTime
		rows2.Scan(&aid, &uname, &phash, &state, &ca, &ua, &lla)
		fmt.Printf("ROW account_id=%q username=%q hash=%q state=%q created=%v updated=%v last=%v\n",
			aid.String, uname.String, phash.String, state.String, ca.Valid, ua.Valid, lla.Valid)
	}
	rows2.Close()
}
