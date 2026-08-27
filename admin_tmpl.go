package main

import (
	"embed"
	"html/template"
)

//go:embed web/view/*.html
var adminFS embed.FS

var adminTmpl = template.Must(template.New("").Funcs(template.FuncMap{
	"formatNum":      formatNum,
	"inputBar":       inputBar,
	"outputBar":      outputBar,
	"callErrorClass": callErrorClass,
	"statusClass":    statusClass,
	"jsAsset":        jsAsset,
	"cssAsset":       cssAsset,
}).ParseFS(adminFS, "web/view/*.html"))
