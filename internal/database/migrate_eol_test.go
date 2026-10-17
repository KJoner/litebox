package database

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 迁移哈希的换行问题:Windows 上 core.autocrlf=true 检出的迁移文件是 CRLF,在那里编出来的
// 二进制把 CRLF 的哈希记进库,Linux 编的二进制随后拒绝打开那份库(反过来也一样),
// 报的是「内容在应用后被修改」,而 SQL 一个字节都没变。下面几条钉住修复的三个方面:
// 换行差异放过、真实改动照样拒绝、库里已有的记录不改写。

func sumHex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// eolHashes 直接从嵌入的文件算每个迁移在 LF 与 CRLF 下的哈希,不经 loadMigrations ——
// 预期值与被测代码出自同一处的话,两边一起错时测试照样是绿的。
func eolHashes(t *testing.T) map[int][2]string {
	t.Helper()
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[int][2]string, len(entries))
	for _, e := range entries {
		var version int
		if _, err := fmt.Sscanf(e.Name(), "%04d_", &version); err != nil {
			t.Fatalf("迁移文件名 %s: %v", e.Name(), err)
		}
		raw, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		lf := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		out[version] = [2]string{sumHex(lf), sumHex(bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n")))}
	}
	return out
}

func recordedHashes(t *testing.T, db *sql.DB) map[int]string {
	t.Helper()
	got, err := appliedMigrations(db)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMigrationHashIgnoresLineEndingsOnly(t *testing.T) {
	want := eolHashes(t)
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations {
		h := want[m.Version]
		if m.Hash != h[0] {
			t.Errorf("%04d 记进库里的哈希应按 LF 算:得到 %s,应为 %s", m.Version, m.Hash[:12], h[0][:12])
		}
		if !m.Matches(h[0]) || !m.Matches(h[1]) {
			t.Errorf("%04d 应同时认 LF 与 CRLF 两种换行下的哈希", m.Version)
		}
		if strings.Contains(m.SQL, "\r") {
			t.Errorf("%04d 执行的 SQL 应统一成 LF", m.Version)
		}
		// 换行之外差一个字节都不行。
		lf := []byte(m.SQL)
		for name, changed := range map[string][]byte{
			"末尾多一个空格":  append(append([]byte{}, lf...), ' '),
			"多一行注释":    append(append([]byte{}, lf...), []byte("-- x\n")...),
			"删掉最后一个字节": lf[:len(lf)-1],
		} {
			crlf := bytes.ReplaceAll(changed, []byte("\n"), []byte("\r\n"))
			if m.Matches(sumHex(changed)) || m.Matches(sumHex(crlf)) {
				t.Errorf("%04d %s之后不应再被认成同一份迁移", m.Version, name)
			}
		}
	}
}

// 已经应用过迁移的旧库,不管当初是哪个平台编的二进制写的,新版本都要能直接打开,
// 而且一条记录都不改写 —— 回滚到旧二进制之后,它还要认得自己当初记下的那一份。
func TestOldDatabasesOpenWhicheverLineEndingTheyRecorded(t *testing.T) {
	hashes := eolHashes(t)
	cases := map[string]func(version int) string{
		"Linux 编的(全是 LF)":              func(v int) string { return hashes[v][0] },
		"Windows autocrlf 编的(全是 CRLF)": func(v int) string { return hashes[v][1] },
		// 真实遇到过的形状:工作区里只有 0018 是 CRLF,其余是 LF。
		"只有 0018 是 CRLF": func(v int) string {
			if v == 18 {
				return hashes[v][1]
			}
			return hashes[v][0]
		},
	}
	for name, hashOf := range cases {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			if err := Migrate(db, nil); err != nil {
				t.Fatal(err)
			}
			// 把记录改成那个平台的二进制当初会写下的样子。
			for v := range recordedHashes(t, db) {
				mustExec(t, db, `UPDATE schema_migrations SET hash = ? WHERE version = ?`, hashOf(v), v)
			}
			before := recordedHashes(t, db)

			if err := Migrate(db, nil); err != nil {
				t.Fatalf("新版本打不开这份旧库: %v", err)
			}
			if err := CheckIntegrity(db); err != nil {
				t.Fatalf("一致性检查: %v", err)
			}
			after := recordedHashes(t, db)
			for v, h := range before {
				if after[v] != h {
					t.Errorf("%04d 的记录被改写了:%s → %s", v, h[:12], after[v][:12])
				}
			}
		})
	}
}

func TestMigrateStillRejectsRealChangesUnderEitherLineEnding(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	var m18 Migration
	for _, m := range migrations {
		if m.Version == 18 {
			m18 = m
		}
	}
	changed := []byte(strings.Replace(m18.SQL, "CREATE", "create", 1))
	if string(changed) == m18.SQL {
		t.Fatal("0018 里没有 CREATE,换一个改法")
	}
	for name, recorded := range map[string]string{
		"改过的内容,LF":   sumHex(changed),
		"改过的内容,CRLF": sumHex(bytes.ReplaceAll(changed, []byte("\n"), []byte("\r\n"))),
	} {
		t.Run(name, func(t *testing.T) {
			db := openTestDB(t)
			if err := Migrate(db, nil); err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `UPDATE schema_migrations SET hash = ? WHERE version = 18`, recorded)
			err := Migrate(db, nil)
			if err == nil || !strings.Contains(err.Error(), "内容在应用后被修改") {
				t.Fatalf("内容真的变了时必须拒绝,得到 %v", err)
			}
		})
	}
}

// 新装的库,Windows 与 Linux 编的二进制写下的是同一份哈希。
func TestNewDatabaseRecordsLFHashes(t *testing.T) {
	hashes := eolHashes(t)
	db := openTestDB(t)
	if err := Migrate(db, nil); err != nil {
		t.Fatal(err)
	}
	got := recordedHashes(t, db)
	if len(got) != len(hashes) {
		t.Fatalf("应用了 %d 个迁移,应为 %d", len(got), len(hashes))
	}
	for v, h := range got {
		if h != hashes[v][0] {
			t.Errorf("%04d 记下的不是 LF 哈希", v)
		}
	}
}

// 迁移文件在任何平台上都按 LF 检出:.gitattributes 里那一行是这件事唯一的保证,
// 嵌入的字节里不该有 CR。后一条失败通常是工作区在加 .gitattributes 之前检出的。
func TestMigrationFilesAreCheckedOutAsLF(t *testing.T) {
	attrs, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatalf("读 .gitattributes: %v", err)
	}
	const rule = "internal/database/migrations/*.sql text eol=lf"
	if !strings.Contains(string(attrs), rule) {
		t.Errorf(".gitattributes 里少了 %q", rule)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("\r")) {
			t.Errorf("%s 含 CR:删掉这个文件再 git checkout 一次,让 .gitattributes 生效", e.Name())
		}
	}
}
