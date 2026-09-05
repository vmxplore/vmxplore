// lamp.go — the LAMP Stack tile: Linux, Apache, MariaDB, PHP.
//
// The Web Stack is the modern shape of the same idea (nginx, PostgreSQL,
// PHP-FPM, a cache). This is the classic one, by the letters, because the
// letters are what people ask for ("is the web app a LAMP?", operator,
// 2026-09-05) and a demo that says LAMP should be one.
//
// Same discipline as the Web Stack: the database on a dataset whose
// recordsize matches its page (InnoDB: 16K), loopback-only listeners,
// credentials in a config the web user can read and nobody else, a live
// example page that opens the database on every load, and a /healthz that
// is 200 only when it did. Verified from clean builds on both families
// before it shipped.
//
// Family differences, checked in containers 2026-09-05:
//
//	Fedora 44   httpd 2.4, php 8.5 (the php package configures php-fpm
//	            behind httpd via /etc/httpd/conf.d/php.conf), php-mysqlnd,
//	            mariadb-server 11.8; units httpd, php-fpm, mariadb
//	Debian 13   apache2 2.4, libapache2-mod-php (PHP 8.4 in-process),
//	            php-mysql, mariadb-server 11.8; units apache2, mariadb
package main

import (
	"fmt"
	"strings"
)

var lampStack = Appliance{
	Name:     "LAMP Stack",
	Summary:  "Apache, MariaDB and PHP — the classic, on its own pool, with a live example page",
	Homepage: "https://httpd.apache.org",
	License:  "Apache-2.0 (httpd), GPL-2.0 (MariaDB), PHP-3.01",

	Distro: "fedora",
	VCPUs:  2,
	RAMMB:  2048,
	DiskGB: 20,

	Needs:  NeedsZFS,
	DataGB: 50,

	Port:    80,
	LandsOn: "http://<vm-ip>/  (stack health at /healthz)",

	Notes: "The stack every PHP application was written for, configured the " +
		"way you would by hand and then verified: Apache serving PHP, " +
		"MariaDB behind it on loopback only, the database on a 16K-record " +
		"dataset matching InnoDB's page size.\n\n" +
		"The landing page is an example application, not a placeholder: " +
		"every load opens the database as the app user, writes a visit and " +
		"reads it back, and shows the instance's hostname, uptime and sizes. " +
		"Drop your own PHP into /var/www/lamp and it is served the same way.\n\n" +
		"/healthz answers 200 only when MariaDB answered a query.",

	Fields: []ApplianceField{
		{Key: "LAMP_POOL", Label: "pool name",
			Placeholder: "created on the appliance's data disk",
			Default:     "tank", Required: true},
		{Key: "LAMP_ALLOW_CIDR", Label: "allowed source",
			Placeholder: "who may reach http",
			Default:     "192.168.0.0/16", Required: true},
		{Key: "LAMP_DB_NAME", Label: "database name", Default: "appdb", Required: true},
		{Key: "LAMP_DB_USER", Label: "database user", Default: "appuser", Required: true},
		{Key: "LAMP_DB_PASS", Label: "database password",
			Placeholder: "blank = generate one", Secret: true,
			Generate: true, Required: true},
	},

	Validate: func(v map[string]string) error {
		if !webStackIdentRE.MatchString(v["LAMP_DB_NAME"]) {
			return fmt.Errorf("database name %q must be lowercase letters, digits and underscores, starting with a letter", v["LAMP_DB_NAME"])
		}
		if !webStackIdentRE.MatchString(v["LAMP_DB_USER"]) {
			return fmt.Errorf("database user %q must be lowercase letters, digits and underscores, starting with a letter", v["LAMP_DB_USER"])
		}
		if len(v["LAMP_DB_PASS"]) < 8 {
			return fmt.Errorf("database password must be at least 8 characters")
		}
		// The password is SQL text once, in the CREATE USER below (quoted,
		// but a quote or whitespace inside it would still make a server
		// that rejects the credential it was built with).
		if strings.ContainsAny(v["LAMP_DB_PASS"], " \t\n'\"\\") {
			return fmt.Errorf("LAMP_DB_PASS must not contain spaces, quotes or backslashes")
		}
		return checkPoolName(v["LAMP_POOL"])
	},

	Script: lampScript,
}

const lampScript = `
APP_TAG=lamp
APP_POOL="$LAMP_POOL"

app_pool_init

# ─── datasets BEFORE packages, so the database's first init lands inside ────
# InnoDB writes 16K pages; a matching recordsize is one page per record.
app_dataset mysql /var/lib/mysql   recordsize=16K
app_dataset www   /var/www         compression=zstd

# ─── one transaction per tier ───────────────────────────────────────────────
if [ "$APP_FAMILY" = rpm ]; then
    app_pkg httpd
    app_pkg mariadb-server
    app_pkg php
    app_pkg php-mysqlnd
    _httpd=httpd; _fpm=php-fpm; _webgrp=apache
    _vhost=/etc/httpd/conf.d/lamp.conf
    # the stock welcome page claims / when there is no index; there is one
    rm -f /etc/httpd/conf.d/welcome.conf
else
    app_pkg apache2
    app_pkg mariadb-server
    app_pkg php
    app_pkg libapache2-mod-php
    app_pkg php-mysql
    _httpd=apache2; _fpm=""; _webgrp=www-data
    _vhost=/etc/apache2/sites-available/lamp.conf
fi

# ─── MariaDB ────────────────────────────────────────────────────────────────
app_selinux mysqld_db_t "/var/lib/mysql(/.*)?"
app_relabel /var/lib/mysql
app_enable mariadb
systemctl restart mariadb 2>/dev/null || true
_i=0
until mysqladmin ping >/dev/null 2>&1; do
    _i=$((_i + 1)); [ "$_i" -lt 30 ] || app_die "MariaDB did not answer within 60 s"
    sleep 2
done
# root over the unix socket (both families' default); the app user over
# TCP on loopback with a password. Idempotent: a re-run resets the password
# to the one this build was given. Identifiers are unquoted on purpose: the
# tile validates them to [a-z][a-z0-9_]*, and a backtick in a heredoc is a
# command substitution waiting to happen.
mysql <<SQL || app_die "database/user setup failed"
CREATE DATABASE IF NOT EXISTS ${LAMP_DB_NAME};
CREATE USER IF NOT EXISTS '${LAMP_DB_USER}'@'127.0.0.1' IDENTIFIED BY '${LAMP_DB_PASS}';
ALTER USER '${LAMP_DB_USER}'@'127.0.0.1' IDENTIFIED BY '${LAMP_DB_PASS}';
GRANT ALL PRIVILEGES ON ${LAMP_DB_NAME}.* TO '${LAMP_DB_USER}'@'127.0.0.1';
FLUSH PRIVILEGES;
SQL

# ─── the example page ───────────────────────────────────────────────────────
install -d -m 0755 /etc/lamp /var/www/lamp
cat >/etc/lamp/config.php <<PHPCONF
<?php
// kldload LAMP Stack — written by the recipe; the example page and /healthz read it.
return ['db_name' => '${LAMP_DB_NAME}', 'db_user' => '${LAMP_DB_USER}', 'db_pass' => '${LAMP_DB_PASS}'];
PHPCONF
chown root:"$_webgrp" /etc/lamp/config.php
chmod 0640 /etc/lamp/config.php
cat >/var/www/lamp/stack.php <<'PHP'
<?php
// stack.php — the check the page and /healthz share.
function lamp_config(): array { return require '/etc/lamp/config.php'; }

function lamp_db(array $c, bool $write): array {
  $t = microtime(true);
  try {
    $pdo = new PDO("mysql:host=127.0.0.1;port=3306;dbname={$c['db_name']};charset=utf8mb4", $c['db_user'], $c['db_pass'],
      [PDO::ATTR_ERRMODE => PDO::ERRMODE_EXCEPTION, PDO::ATTR_TIMEOUT => 3]);
    $pdo->exec('CREATE TABLE IF NOT EXISTS visits (id INT AUTO_INCREMENT PRIMARY KEY, at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP, client VARCHAR(64), host VARCHAR(64))');
    if ($write) {
      $st = $pdo->prepare('INSERT INTO visits (client, host) VALUES (?, ?)');
      $st->execute([$_SERVER['REMOTE_ADDR'] ?? '', gethostname()]);
    }
    $count = (int)$pdo->query('SELECT COUNT(*) FROM visits')->fetchColumn();
    $last = $pdo->query('SELECT MAX(at) FROM visits')->fetchColumn();
    $ver = $pdo->query('SELECT VERSION()')->fetchColumn();
    return ['ok' => true, 'version' => 'MariaDB ' . $ver, 'visits' => $count, 'last' => $last,
            'ms' => round((microtime(true) - $t) * 1000, 1)];
  } catch (Throwable $e) {
    return ['ok' => false, 'error' => $e->getMessage()];
  }
}
PHP
cat >/var/www/lamp/healthz.php <<'PHP'
<?php
// /healthz — 200 only when MariaDB answered a query; reads, never writes.
require '/var/www/lamp/stack.php';
$db = lamp_db(lamp_config(), false);
http_response_code($db['ok'] ? 200 : 503);
header('Content-Type: application/json');
echo json_encode(['ok' => $db['ok'], 'host' => gethostname(), 'mariadb' => $db]), "\n";
PHP
cat >/var/www/lamp/index.php <<'PHP'
<?php
// The example page: a visit is a GET of the page itself, nothing else counts.
require '/var/www/lamp/stack.php';
$visit = ($_SERVER['REQUEST_METHOD'] ?? '') === 'GET' && strtok($_SERVER['REQUEST_URI'] ?? '/', '?') === '/';
$db = lamp_db(lamp_config(), $visit);
$host = gethostname();
$ip = $_SERVER['SERVER_ADDR'] ?? '';
$up = (int)explode(' ', (string)@file_get_contents('/proc/uptime'))[0];
$uptime = sprintf('%dd %02dh %02dm', intdiv($up, 86400), intdiv($up % 86400, 3600), intdiv($up % 3600, 60));
$cores = max(1, (int)substr_count((string)@file_get_contents('/proc/cpuinfo'), "\nprocessor"), (int)str_starts_with((string)@file_get_contents('/proc/cpuinfo'), 'processor'));
$mem = 0; if (preg_match('/MemTotal:\s+(\d+)/', (string)@file_get_contents('/proc/meminfo'), $m)) { $mem = round($m[1] / 1024); }
$mark = fn(bool $ok) => $ok ? '<span class="ok">●</span>' : '<span class="bad">●</span>';
$h = fn($v) => htmlspecialchars((string)$v, ENT_QUOTES);
?>
<!doctype html>
<!-- lamp up -->
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>LAMP · <?= $h($host) ?></title>
<style>
:root{color-scheme:dark}body{margin:0;background:#101418;color:#d7dde5;font:15px/1.5 system-ui,sans-serif}
main{max-width:860px;margin:0 auto;padding:2.5rem 1.5rem}h1{font-size:1.6rem;margin:0 0 .2rem}h1 small{color:#8a94a3;font-weight:400;font-size:1rem;margin-left:.6rem}
.sub{color:#8a94a3;margin:0 0 2rem}.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(250px,1fr));gap:1rem}
.card{background:#171c23;border:1px solid #242b35;border-radius:10px;padding:1rem 1.2rem}.card h2{font-size:.85rem;letter-spacing:.06em;text-transform:uppercase;color:#8a94a3;margin:0 0 .6rem}
.big{font-size:2rem;font-weight:600;margin:.1rem 0}.kv{display:grid;grid-template-columns:auto 1fr;gap:.15rem .8rem;font-family:ui-monospace,monospace;font-size:.85rem}.kv b{color:#8a94a3;font-weight:400}
.ok{color:#5fd38d}.bad{color:#ff6b6b}.err{color:#ff6b6b;font-family:ui-monospace,monospace;font-size:.85rem}
footer{margin-top:2rem;color:#8a94a3;font-size:.85rem}footer a{color:#8fb4ff}code{color:#c9d3e0}
</style></head><body><main>
<h1>LAMP <small><?= $h($host) ?></small></h1>
<p class="sub">Linux · Apache · MariaDB · PHP, on its own ZFS pool. Every visit writes a row — reload to watch.</p>
<div class="grid">
<div class="card"><h2>this instance</h2>
<div class="kv"><b>host</b><span><?= $h($host) ?></span><b>address</b><span><?= $h($ip) ?></span><b>uptime</b><span><?= $uptime ?></span>
<b>cpu / ram</b><span><?= $cores ?> core<?= $cores == 1 ? '' : 's' ?> / <?= $mem ?> MB</span><b>kernel</b><span><?= $h(php_uname('r')) ?></span></div></div>
<div class="card"><h2><span class="ok">●</span> Apache + PHP</h2>
<div class="kv"><b>server</b><span><?= $h($_SERVER['SERVER_SOFTWARE'] ?? 'Apache') ?></span><b>php</b><span><?= PHP_VERSION ?> via <?= $h(PHP_SAPI) ?></span><b>docroot</b><span>/var/www/lamp</span></div></div>
<div class="card"><h2><?= $mark($db['ok']) ?> MariaDB</h2>
<?php if ($db['ok']): ?><div class="big"><?= $db['visits'] ?></div><div class="kv"><b>visits</b><span>rows in <code>visits</code></span><b>last</b><span><?= $h($db['last']) ?></span><b>server</b><span><?= $h($db['version']) ?></span><b>round trip</b><span><?= $db['ms'] ?> ms</span></div>
<?php else: ?><p class="err"><?= $h($db['error']) ?></p><?php endif ?></div>
</div>
<footer><a href="/healthz">/healthz</a> answers 200 only when MariaDB does · built by <a href="https://kldload.com">kldload</a></footer>
</main></body></html>
PHP
chown -R root:"$_webgrp" /var/www/lamp
chmod 0755 /var/www/lamp
chmod 0644 /var/www/lamp/*.php

# ─── Apache ─────────────────────────────────────────────────────────────────
cat >"$_vhost" <<VHOST
# kldload LAMP Stack — the one site this VM serves
<VirtualHost *:80>
    DocumentRoot /var/www/lamp
    DirectoryIndex index.php
    Alias /healthz /var/www/lamp/healthz.php
    <Directory /var/www/lamp>
        Require all granted
        AllowOverride None
    </Directory>
    <Files "stack.php">
        Require all denied
    </Files>
    ServerSignature Off
</VirtualHost>
ServerTokens Prod
VHOST
if [ "$APP_FAMILY" != rpm ]; then
    a2dissite 000-default >/dev/null 2>&1 || true
    a2ensite lamp >/dev/null 2>&1 || app_die "a2ensite lamp failed"
fi
# SELinux confines httpd and php-fpm (both httpd_t): the page's TCP
# connection to MariaDB on loopback is refused without this boolean.
# name=value pairs — the other spelling is a usage error (Web Stack, 2026-09-05).
if command -v setsebool >/dev/null 2>&1 && selinuxenabled 2>/dev/null; then
    setsebool -P httpd_can_network_connect_db=on ||
        app_die "setsebool failed — PHP could not reach MariaDB under SELinux"
fi
app_selinux httpd_sys_content_t "/var/www/lamp(/.*)?"
app_relabel /var/www
if [ -n "$_fpm" ]; then
    app_enable "$_fpm"
    systemctl restart "$_fpm" 2>/dev/null || true
fi
if command -v apachectl >/dev/null 2>&1; then _ctl=apachectl; else _ctl=apache2ctl; fi
"$_ctl" configtest >/dev/null 2>&1 || { "$_ctl" configtest; app_die "Apache config does not parse"; }
app_enable "$_httpd"
systemctl restart "$_httpd"

# ─── firewall, verify ───────────────────────────────────────────────────────
app_firewall lamp "$LAMP_ALLOW_CIDR" 80/tcp

export LAMP_DB_NAME LAMP_DB_USER LAMP_DB_PASS 2>/dev/null || true
echo
app_check "mariadb answers"         mysqladmin ping
app_check "database exists"        bash -c 'mysql -N -e "SHOW DATABASES LIKE '"'"'$LAMP_DB_NAME'"'"'" | grep -q .'
app_check "app user can connect"   bash -c 'mysql -h 127.0.0.1 -u "$LAMP_DB_USER" -p"$LAMP_DB_PASS" -N -e "SELECT 1" "$LAMP_DB_NAME" | grep -q 1'
app_check "healthz: MariaDB answers" bash -c 'curl -fsS http://127.0.0.1/healthz | grep -q "\"ok\":true"'
app_check "page queries the database" bash -c 'curl -fsS http://127.0.0.1/ | grep -q "lamp up"'
app_check "page wrote a visit"     bash -c 'mysql -h 127.0.0.1 -u "$LAMP_DB_USER" -p"$LAMP_DB_PASS" -N -e "SELECT COUNT(*) FROM visits" "$LAMP_DB_NAME" | grep -qE "^[1-9]"'
app_check "stack.php is not served" bash -c '[ "$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1/stack.php)" = 403 ]'
app_check "mariadb enabled"        systemctl is-enabled mariadb
app_check "apache enabled"         systemctl is-enabled "$_httpd"
[ -z "$_fpm" ] || app_check "php-fpm enabled" systemctl is-enabled "$_fpm"
if [ -n "${APP_POOL:-}" ]; then
    app_check "mysql recordsize 16K" bash -c '[ "$(zfs get -H -o value recordsize "$APP_POOL"/mysql)" = 16K ]'
    app_snapshot postinstall-lamp
fi

cat <<EOM

  LAMP Stack

  Site        http://$(hostname -I 2>/dev/null | awk '{print $1}')/   (the example page: a visit row per load)
  Health      /healthz  — 200 only when MariaDB answers
  Database    ${LAMP_DB_NAME} owner ${LAMP_DB_USER}  (127.0.0.1:3306)
  Docroot     /var/www/lamp
  Data        pool: ${APP_POOL:-<none — plain dirs>}
  Firewall    zone 'lamp', source ${LAMP_ALLOW_CIDR}

EOM
app_summary
`
