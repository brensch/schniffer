package main

import (
	"embed"
	"fmt"
	"html/template"
	"os"
)

//go:embed internal/web/assets/*
var assets embed.FS

func main() {
	// Read files
	cssBytes, _ := assets.ReadFile("internal/web/assets/style.css")
	jsBytes, _ := assets.ReadFile("internal/web/assets/app.js")
	htmlBytes, _ := assets.ReadFile("internal/web/assets/index.html")

	fmt.Printf("CSS: %d bytes\n", len(cssBytes))
	fmt.Printf("JS: %d bytes\n", len(jsBytes))
	fmt.Printf("HTML: %d bytes\n", len(htmlBytes))

	// Parse and execute template
	tmpl, err := template.New("test").Parse(string(htmlBytes))
	if err != nil {
		panic(err)
	}

	data := struct {
		CSS string
		JS  string
	}{
		CSS: string(cssBytes),
		JS:  string(jsBytes),
	}

	file, err := os.Create("output.html")
	if err != nil {
		panic(err)
	}
	defer file.Close()

	err = tmpl.Execute(file, data)
	if err != nil {
		panic(err)
	}

	fmt.Println("Output written to output.html")
}
