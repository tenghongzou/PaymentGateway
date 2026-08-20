// Package migrations 以 embed.FS 內嵌所有服務的 SQL migration，讓服務二進位自帶 schema
// （PG_AUTO_MIGRATE 與 `/app migrate up|down|version` 子命令皆讀此處，docs/07 §1.8）。
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
)

// FS 內嵌 migrations/<service>/*.sql。
//
//go:embed merchant/*.sql payment/*.sql ledger/*.sql webhook/*.sql reconciliation/*.sql
var FS embed.FS

// Services 為擁有 migration 目錄的服務短名。
var Services = []string{"merchant", "payment", "ledger", "webhook", "reconciliation"}

// Source 回傳某服務的 migration 子目錄（供 golang-migrate 的 iofs source 使用）。
func Source(service string) (fs.FS, error) {
	sub, err := fs.Sub(FS, service)
	if err != nil {
		return nil, fmt.Errorf("migrations: no directory for service %q: %w", service, err)
	}
	entries, err := fs.ReadDir(sub, ".")
	if err != nil || len(entries) == 0 {
		return nil, fmt.Errorf("migrations: service %q has no migration files", service)
	}
	return sub, nil
}
