package main

import (
	"embed"
	"os"

	"newapi-subscription-redeemer/internal/redeemer"
)

//go:embed web/*
var webFS embed.FS

func main() {
	os.Exit(redeemer.Main(os.Args[1:], webFS))
}
