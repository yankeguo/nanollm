package main

import (
	"embed"
	"html/template"
)

//go:embed admin/*.html
var adminFS embed.FS

var adminTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"formatNum":      formatNum,
	"inputBar":       inputBar,
	"outputBar":      outputBar,
	"callErrorClass": callErrorClass,
	"statusClass":    statusClass,
	"setFilter":      setFilter,
	"filterPath":     filterPath,
	"filterQuery":    filterQuery,
}).ParseFS(adminFS, "admin/*.html"))
