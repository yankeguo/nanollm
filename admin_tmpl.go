package main

import (
	"embed"
	"html/template"
)

//go:embed admin/*.html
var adminFS embed.FS

var adminTmpl = template.Must(template.ParseFS(adminFS, "admin/*.html"))
