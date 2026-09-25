package main

import (
	"fmt"
	"net/http"
	"os"
)

func main() {
	v := os.Getenv("VERSION")
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "hello", v) })
	fmt.Println("listening, version", v)
	_ = http.ListenAndServe(":8080", nil)
}
