package main

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"time"

	"github.com/kolesnikav/icloud-synk/internal/config"
	"github.com/kolesnikav/icloud-synk/internal/icloud"
)

func main() {
	cfgDir, err := config.Dir()
	if err != nil {
		log.Fatal(err)
	}

	client, err := icloud.New(filepath.Join(cfgDir, "session.json"))
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := client.Authenticate(ctx); err != nil {
		log.Fatalf("iCloud-Anmeldung fehlgeschlagen: %v", err)
	}

	target := "Tappe/Dokumente sync NAS"
	currentID := icloud.RootFolderID

	// Zielordner anhand seiner Komponenten suchen.
	for _, component := range strings.Split(target, "/") {
		folders, err := client.ListFolders(ctx, []string{currentID})
		if err != nil {
			log.Fatalf("Ordnerabfrage fehlgeschlagen: %v", err)
		}

		found := false
		for _, folder := range folders {
			if folder.DrivewsID != currentID {
				continue
			}

			for _, item := range folder.Items {
				if item.FullName() == component && item.IsDir() {
					fmt.Printf("Gefunden: %s\n", component)
					currentID = item.DrivewsID
					found = true
					break
				}
			}
			if found {
				break
			}
		}

		if !found {
			log.Fatalf("Nicht gefunden: %s (gesuchter Pfad: %s)", component, target)
		}
	}

	fmt.Printf("\nLese Inhalt von: %s\n", target)

	var files, dirs int
	queue := []string{currentID}
	visited := make(map[string]bool)

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			log.Fatalf("Prüfung abgebrochen: %v", err)
		}

		id := queue[0]
		queue = queue[1:]

		if visited[id] {
			continue
		}
		visited[id] = true

		folders, err := client.ListFolders(ctx, []string{id})
		if err != nil {
			log.Fatalf("Inhalt konnte nicht gelesen werden: %v", err)
		}

		for _, folder := range folders {
			if folder.DrivewsID != id {
				continue
			}

			for _, item := range folder.Items {
				if item.IsDir() {
					dirs++
					queue = append(queue, item.DrivewsID)
				} else if item.Type == icloud.TypeFile {
					files++
				}
			}
		}
	}

	fmt.Printf("Dateien: %d\n", files)
	fmt.Printf("Unterordner: %d\n", dirs)
	fmt.Println("Nur gelesen; keine Dateien übertragen oder gelöscht.")
}
