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
			log.Fatalf("Ordner nicht gefunden: %s", component)
		}
	}

	fmt.Println("iCloud-Metadaten (nur lesend):")
	queue := []string{currentID}
	visited := make(map[string]bool)

	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			log.Fatalf("Diagnose abgebrochen: %v", err)
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
					queue = append(queue, item.DrivewsID)
					continue
				}

				if item.FullName() == "icloud-test.txt" ||
					strings.Contains(strings.ToLower(item.FullName()), "test-von-icloud") {
					fmt.Printf(
						"Datei: %s | Typ: %s | Größe: %d | ETag: %s | Geändert: %s | ItemCount: %d\n",
						item.FullName(),
						item.Type,
						item.Size,
						item.Etag,
						item.ModTime().Format(time.RFC3339),
						item.ItemCount,
					)
				}
			}
		}
	}

	fmt.Println("Diagnose beendet. Es wurden keine Dateien übertragen oder gelöscht.")
}
