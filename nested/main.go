package main

import (
	"fmt"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

func main() {
	title := cases.Title(language.English)
	fmt.Println(title.String("only dependency bump"))
}
