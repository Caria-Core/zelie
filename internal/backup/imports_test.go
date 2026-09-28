package backup

import (
	"bytes"
	"compress/gzip"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/Caria-Core/zelie/internal/msg"
	"github.com/klauspost/compress/zstd"
)

// Cut from a mysqldump of a Pelican panel's database, made with
// --databases, --routines and --triggers.
const mysqldump = "-- MariaDB dump 10.19  Distrib 10.11.6-MariaDB, for debian-linux-gnu (x86_64)\n" +
	"--\n-- Host: localhost    Database: panel\n" +
	"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;\n" +
	"SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;\n" +
	"SET @@SESSION.SQL_LOG_BIN= 0;\n" +
	"SET @@GLOBAL.GTID_PURGED=/*!80000 '+'*/ '3E11FA47-71CA-11E1-9E33-C80AA9429562:1-5,\n" +
	"4E11FA47-71CA-11E1-9E33-C80AA9429562:1-9';\n" +
	"\n" +
	"CREATE DATABASE /*!32312 IF NOT EXISTS*/ `panel` /*!40100 DEFAULT CHARACTER SET utf8mb4 */;\n" +
	"\n" +
	"USE `panel`;\n" +
	"DROP TABLE IF EXISTS `users`;\n" +
	"CREATE TABLE `users` (\n" +
	"  `id` int(10) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `note` text,\n" +
	"  PRIMARY KEY (`id`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n" +
	"INSERT INTO `users` VALUES (1,'USE `panel`;'),(2,'DEFINER=`root`@`localhost`');\n" +
	"/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
	"/*!50013 DEFINER=`root`@`localhost` SQL SECURITY DEFINER */\n" +
	"/*!50001 VIEW `active` AS select `users`.`id` AS `id` from `users` */;\n" +
	"DELIMITER ;;\n" +
	"/*!50003 CREATE*/ /*!50017 DEFINER=`pelican`@`%`*/ /*!50003 TRIGGER `stamp` BEFORE INSERT ON `users` FOR EACH ROW SET NEW.note = 'x' */;;\n" +
	"CREATE DEFINER=`root`@`localhost` PROCEDURE `count_users`()\n" +
	"BEGIN\n  SELECT COUNT(*) FROM users;\nEND ;;\n" +
	"DELIMITER ;\n" +
	"SET @@SESSION.SQL_LOG_BIN = @MYSQLDUMP_TEMP_LOG_BIN;\n" +
	"-- Dump completed on 2026-09-20 12:00:00\n"

const mysqldumpWant = "-- MariaDB dump 10.19  Distrib 10.11.6-MariaDB, for debian-linux-gnu (x86_64)\n" +
	"--\n-- Host: localhost    Database: panel\n" +
	"/*!40101 SET @OLD_CHARACTER_SET_CLIENT=@@CHARACTER_SET_CLIENT */;\n" +
	"SET @MYSQLDUMP_TEMP_LOG_BIN = @@SESSION.SQL_LOG_BIN;\n" +
	"\n\n" +
	"DROP TABLE IF EXISTS `users`;\n" +
	"CREATE TABLE `users` (\n" +
	"  `id` int(10) unsigned NOT NULL AUTO_INCREMENT,\n" +
	"  `note` text,\n" +
	"  PRIMARY KEY (`id`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n" +
	"INSERT INTO `users` VALUES (1,'USE `panel`;'),(2,'DEFINER=`root`@`localhost`');\n" +
	"/*!50001 CREATE ALGORITHM=UNDEFINED */\n" +
	"/*!50013  SQL SECURITY DEFINER */\n" +
	"/*!50001 VIEW `active` AS select `users`.`id` AS `id` from `users` */;\n" +
	"DELIMITER ;;\n" +
	"/*!50003 CREATE*/ /*!50017 */ /*!50003 TRIGGER `stamp` BEFORE INSERT ON `users` FOR EACH ROW SET NEW.note = 'x' */;;\n" +
	"CREATE  PROCEDURE `count_users`()\n" +
	"BEGIN\n  SELECT COUNT(*) FROM users;\nEND ;;\n" +
	"DELIMITER ;\n" +
	"-- Dump completed on 2026-09-20 12:00:00\n"

// A plain pg_dump made on another server, where a role named shop owned
// everything.
const pgDump = "--\n-- PostgreSQL database dump\n--\n\n" +
	"SET statement_timeout = 0;\n" +
	"\\connect shop\n" +
	"CREATE SCHEMA extra;\n" +
	"ALTER SCHEMA extra OWNER TO shop;\n" +
	"CREATE FUNCTION public.touch() RETURNS trigger\n" +
	"    LANGUAGE plpgsql\n" +
	"    AS $$\n" +
	"BEGIN\n" +
	"GRANT SELECT ON orders TO reporting;\n" +
	"DROP TABLE IF EXISTS scratch;\n" +
	"RETURN NEW;\n" +
	"END;\n" +
	"$$;\n" +
	"ALTER FUNCTION public.touch() OWNER TO shop;\n" +
	"CREATE TABLE public.orders (\n" +
	"    id integer NOT NULL,\n" +
	"    note text\n" +
	");\n" +
	"ALTER TABLE public.orders OWNER TO shop;\n" +
	"COPY public.orders (id, note) FROM stdin;\n" +
	"1\tGRANT ALL ON orders TO shop;\n" +
	"2\tALTER TABLE x OWNER TO y;\n" +
	"\\.\n" +
	"\n" +
	"REVOKE ALL ON SCHEMA public FROM PUBLIC;\n" +
	"GRANT SELECT ON TABLE public.orders\n" +
	"    TO reporting;\n" +
	"ALTER DEFAULT PRIVILEGES FOR ROLE shop IN SCHEMA public GRANT SELECT ON TABLES TO reporting;\n" +
	"\n--\n-- PostgreSQL database dump complete\n--\n"

const pgDumpWant = "--\n-- PostgreSQL database dump\n--\n\n" +
	"SET statement_timeout = 0;\n" +
	"CREATE SCHEMA extra;\n" +
	"CREATE FUNCTION public.touch() RETURNS trigger\n" +
	"    LANGUAGE plpgsql\n" +
	"    AS $$\n" +
	"BEGIN\n" +
	"GRANT SELECT ON orders TO reporting;\n" +
	"DROP TABLE IF EXISTS scratch;\n" +
	"RETURN NEW;\n" +
	"END;\n" +
	"$$;\n" +
	"CREATE TABLE public.orders (\n" +
	"    id integer NOT NULL,\n" +
	"    note text\n" +
	");\n" +
	"COPY public.orders (id, note) FROM stdin;\n" +
	"1\tGRANT ALL ON orders TO shop;\n" +
	"2\tALTER TABLE x OWNER TO y;\n" +
	"\\.\n" +
	"\n" +
	"\n--\n-- PostgreSQL database dump complete\n--\n"

func importString(t *testing.T, kind string, in []byte) (string, Adapted, error) {
	t.Helper()
	var out bytes.Buffer
	a, err := ImportDump(kind, bytes.NewReader(in), &out)
	return out.String(), a, err
}

func TestImportMariaDB(t *testing.T) {
	got, a, err := importString(t, "mariadb", []byte(mysqldump))
	if err != nil {
		t.Fatal(err)
	}
	if got != mysqldumpWant {
		t.Errorf("got:\n%s\nwant:\n%s", got, mysqldumpWant)
	}
	want := Adapted{"SQL_LOG_BIN": 2, "GTID_PURGED": 1, "CREATE DATABASE": 1, "USE": 1, "DEFINER": 3}
	if !maps.Equal(a, want) {
		t.Errorf("adapted %v, want %v", a, want)
	}
}

func TestImportPostgres(t *testing.T) {
	got, a, err := importString(t, "postgres", []byte(pgDump))
	if err != nil {
		t.Fatal(err)
	}
	if got != pgDumpWant {
		t.Errorf("got:\n%s\nwant:\n%s", got, pgDumpWant)
	}
	want := Adapted{`\connect`: 1, "OWNER TO": 3, "REVOKE": 1, "GRANT": 1, "DEFAULT PRIVILEGES": 1}
	if !maps.Equal(a, want) {
		t.Errorf("adapted %v, want %v", a, want)
	}
}

func TestImportCompressed(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(mysqldump))
	zw.Close()
	var zs bytes.Buffer
	enc, _ := zstd.NewWriter(&zs)
	enc.Write([]byte(mysqldump))
	enc.Close()
	for name, in := range map[string][]byte{"gzip": gz.Bytes(), "zstd": zs.Bytes()} {
		got, _, err := importString(t, "mariadb", in)
		if err != nil || got != mysqldumpWant {
			t.Errorf("%s: %v\n%s", name, err, got)
		}
	}
}

// pg_dump -C from PostgreSQL 16.10 and later: \connect sits between
// \unrestrict and \restrict, which stay.
func TestImportPgRestrict(t *testing.T) {
	in := "\\restrict k\n\nCREATE DATABASE shop WITH TEMPLATE = template0;\n\n\nALTER DATABASE shop OWNER TO shop;\n\n" +
		"\\unrestrict k\n\\connect shop\n\\restrict k\n\nSET lock_timeout = 0;\nALTER TABLE t OWNER TO shop;\n\\unrestrict k\n"
	want := "\\restrict k\n\n\n\n\n\\unrestrict k\n\\restrict k\n\nSET lock_timeout = 0;\n\\unrestrict k\n"
	got, a, err := importString(t, "postgres", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	if !maps.Equal(a, Adapted{"CREATE DATABASE": 1, "ALTER DATABASE": 1, `\connect`: 1, "OWNER TO": 1}) {
		t.Errorf("adapted %v", a)
	}
}

// Windows line ends and a last line without one are kept as they are.
func TestImportLineEnds(t *testing.T) {
	in := "USE `old`;\r\nCREATE TABLE t (id int);\r\nINSERT INTO t VALUES (1);"
	got, a, err := importString(t, "mariadb", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if want := "CREATE TABLE t (id int);\r\nINSERT INTO t VALUES (1);"; got != want || a["USE"] != 1 {
		t.Errorf("got %q %v", got, a)
	}
}

// A line longer than the reader's buffer is data and goes through whole;
// one inside a dropped statement goes with it.
func TestImportLongLines(t *testing.T) {
	long := "INSERT INTO t VALUES ('" + strings.Repeat("x", 3*maxAdaptLine) + "');\n"
	in := "SET @@GLOBAL.GTID_PURGED='a:1,\n" + strings.Repeat("b", 2*maxAdaptLine) + "\n';\n" + long + "USE `x`;\n"
	got, a, err := importString(t, "mariadb", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if got != long {
		t.Errorf("got %d bytes, want %d", len(got), len(long))
	}
	if a["GTID_PURGED"] != 1 || a["USE"] != 1 {
		t.Errorf("adapted %v", a)
	}
}

func TestImportRedis(t *testing.T) {
	rdb := []byte("REDIS0011\xfa\x09redis-ver\x057.4.0\xff")
	got, _, err := importString(t, "redis", rdb)
	if err != nil || got != string(rdb) {
		t.Fatalf("%v %q", err, got)
	}
}

func TestImportRefuses(t *testing.T) {
	for _, c := range []struct {
		kind, in, code string
	}{
		{"mariadb", "", "import.empty"},
		{"postgres", "PGDMP\x01\x0e\x00", "import.pg_custom"},
		{"mariadb", "PGDMP\x01\x0e\x00", "import.other_engine"},
		{"postgres", "--\n-- PostgreSQL database cluster dump\n--\n", "import.pg_cluster"},
		{"mariadb", "--\n-- PostgreSQL database dump\n--\n", "import.other_engine"},
		{"postgres", "-- MySQL dump 10.13\n", "import.other_engine"},
		{"postgres", "-- phpMyAdmin SQL Dump\n", "import.other_engine"},
		{"mariadb", "PK\x03\x04\x14\x00", "import.zip"},
		{"mariadb", "ustar\x00\x00\x00", "import.not_sql"},
		{"redis", "-- MySQL dump\n", "import.not_redis"},
		{"mariadb", "\x1f\x8b\x08\x00broken", "import.damaged"},
	} {
		_, _, err := importString(t, c.kind, []byte(c.in))
		var me *msg.Error
		if !errors.As(err, &me) || me.Code != c.code {
			t.Errorf("%s %q: %v, want %s", c.kind, c.in, err, c.code)
		}
	}
}

// A gzip file cut short is damaged, not a shorter dump.
func TestImportCutGzip(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(strings.Repeat(mysqldump, 50)))
	zw.Close()
	_, _, err := importString(t, "mariadb", gz.Bytes()[:gz.Len()/2])
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != "import.damaged" {
		t.Fatalf("%v", err)
	}
}
