# Dokumentation: Erkennung entfernter Größenänderungen

**Datum:** 17. Oktober 2026
**Projekt:** icloud-synk
**Betroffene Implementierung:** `internal/synk/reconcile.go`

## 1. Ausgangsproblem

Die Funktion `remoteChanged()` nutzte die ETag-Kennung als maßgebliches
Kriterium für Änderungen an entfernten Dateien. Wenn die ETag-Kennung
unverändert blieb, obwohl sich die Dateigröße änderte, konnte die
Änderung unentdeckt bleiben.

## 2. Umsetzung

Die Funktion prüft nun:

1. Ohne gespeicherten Zustand gilt die Datei als geändert.
2. Ohne gespeicherte ETag-Kennung gilt die Datei als geändert.
3. Eine abweichende ETag-Kennung signalisiert eine Änderung.
4. Bei unveränderter ETag-Kennung wird zusätzlich die Dateigröße verglichen.

Die Reconciliation-Logik entscheidet weiterhin, ob ein Download, Upload
oder eine Konfliktbehandlung erforderlich ist.

## 3. Regressionstests

In `internal/synk/reconcile_test.go` wurden zwei Fälle ergänzt:

- `TestReconcileRemoteSizeChangeDownloadsWhenETagUnchanged`
  Prüft, dass eine entfernte Größenänderung bei unveränderter ETag-Kennung
  einen Download auslöst, wenn lokal keine Änderung vorliegt.

- `TestReconcileConflictWhenRemoteSizeChangesAndETagDoesNot`
  Prüft die Konfliktbehandlung, wenn sowohl die lokale als auch die
  entfernte Datei gegenüber dem gespeicherten Zustand geändert wurden.

## 4. Testergebnisse

Am 17. Oktober 2026 wurden folgende Prüfungen erfolgreich ausgeführt:

- `go test -count=1 ./...`
- `go vet ./...`
- `git diff --check`

Der installierte Daemon lud anschließend zwei Dateien herunter:

- `icloud-synk-test-20261012-210710.txt` (52 Byte)
- `konflikt-test.txt` (17 Byte)

Der anschließende read-only Vergleich meldete 32 lokale Dateien,
32 Dateien in iCloud und 32 identische Dateien.

Dieses Ergebnis beschreibt den geprüften Zustand zu diesem Zeitpunkt.
Es ist kein Beweis für alle denkbaren Konfliktfälle.

## 5. Grenzen

Eine Inhaltsänderung bei gleicher Dateigröße und unveränderter ETag-Kennung
wird durch den zusätzlichen Größenvergleich nicht erkannt.

Die Prüfung setzt außerdem voraus, dass die iCloud-Schnittstelle die
Dateigröße zuverlässig liefert und der gespeicherte Zustand einen
vergleichbaren Wert enthält.

## 6. Sicherungsstand

Vollständiges Projektarchiv:

`/home/magic/icloud-synk-sicherung/projekt-komplett-20261017-165934-nach-erfolgreichem-verify.tar.gz`

SHA-256:

`a82880cb8d9a680c49e7d5017b34515c453fac82f062d0d43e2ae9f283c41fcc`

Das Archiv wurde geprüft, bevor diese Dokumentation erstellt wurde.
Die Dokumentation ist daher nicht Bestandteil dieses Archivs.

## 7. Git-Stand

Die Änderungen wurden inzwischen in zwei separaten Commits versioniert:

- `4851ebb` – `fix: detect remote size changes with unchanged ETag`
- `17f9e86` – `feat: add read-only Drive verification command`

Die Dokumentation selbst wurde nach Erstellung des Projektarchivs angelegt.
Weitere unversionierte Dateien und Sicherungskopien wurden beibehalten
und sind nicht Bestandteil dieser Commits.
