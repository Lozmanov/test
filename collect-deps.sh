#!/bin/bash
# Собирает все runtime-зависимости (JRE + системные .so) в /rootfs
# для последующего копирования в образ FROM scratch.
set -euo pipefail

ROOTFS="${1:-/rootfs}"
mkdir -p "$ROOTFS"

# ── Вспомогательные функции ─────────────────────────────────────────────────

# Копирует файл в ROOTFS, сохраняя путь. Разыменовывает один уровень симлинков.
copy_file() {
    local src="$1"
    [ -e "$src" ] || return 0
    local real; real="$(readlink -f "$src")"
    local dest_dir="${ROOTFS}$(dirname "$real")"
    mkdir -p "$dest_dir"
    [ -f "${dest_dir}/$(basename "$real")" ] || cp -p "$real" "$dest_dir/"
    # Если src — симлинк, воссоздаём его в ROOTFS
    if [ -L "$src" ] && [ "$src" != "$real" ]; then
        local link_dir="${ROOTFS}$(dirname "$src")"
        local link_name; link_name="$(basename "$src")"
        mkdir -p "$link_dir"
        [ -e "${link_dir}/${link_name}" ] || ln -sf "$real" "${link_dir}/${link_name}"
    fi
}

# Копирует бинарник и все его динамические зависимости (.so).
copy_with_deps() {
    local bin="$1"
    local real; real="$(readlink -f "$bin")"
    copy_file "$bin"
    # ldd: строки вида "libname => /path (addr)" и "/path/ld-linux (addr)"
    ldd "$real" 2>/dev/null | while read -r line; do
        local lib
        lib="$(echo "$line" | awk '/=>/ {print $3}')"
        [ -z "$lib" ] && lib="$(echo "$line" | awk 'NF==2 && /^\// {print $1}')"
        [ -z "$lib" ] || [ "$lib" = "not" ] && continue
        [ -f "$lib" ] || continue
        copy_file "$lib"
    done
}

# ── 1. Java ──────────────────────────────────────────────────────────────────
echo ">>> Java binary + dynamic deps"
JAVA_REAL="$(readlink -f "$(which java)")"
copy_with_deps "$JAVA_REAL"

# Симлинк /usr/bin/java → реальный путь
mkdir -p "${ROOTFS}/usr/bin"
[ -e "${ROOTFS}/usr/bin/java" ] || ln -sf "$JAVA_REAL" "${ROOTFS}/usr/bin/java"

# ── 2. JVM-внутренние .so (libjvm.so, libjava.so, …) ────────────────────────
echo ">>> JVM internal libraries"
# Спрашиваем у самого JVM его java.home — надёжнее, чем идти от пути бинарника.
# java.home в Java 8 указывает на директорию JRE (там лежит lib/tzdb.dat, lib/rt.jar и т.д.)
JAVA_HOME_PROP=$(java -XshowSettings:property -version 2>&1 \
    | awk -F'=' '/java\.home/ {gsub(/ /,"",$2); print $2; exit}')

# Если не удалось — fallback: двигаемся вверх от бинарника
if [ -z "$JAVA_HOME_PROP" ] || [ ! -d "$JAVA_HOME_PROP" ]; then
    JVM_BIN_DIR="$(dirname "$JAVA_REAL")"
    JVM_ROOT="$(dirname "$JVM_BIN_DIR")"
    for candidate in "${JVM_ROOT}/jre" "${JVM_ROOT}"; do
        if [ -d "${candidate}/lib" ]; then
            JAVA_HOME_PROP="${candidate}"
            break
        fi
    done
fi

JVM_LIB_DIR="${JAVA_HOME_PROP}/lib"
echo "  JVM home: ${JAVA_HOME_PROP}"
echo "  JVM lib dir: ${JVM_LIB_DIR}"

mkdir -p "${ROOTFS}${JVM_LIB_DIR}"
cp -r "${JVM_LIB_DIR}/." "${ROOTFS}${JVM_LIB_DIR}/"

# Зависимости всех JVM .so файлов (оригиналы, не из ROOTFS)
find "${JVM_LIB_DIR}" -name "*.so*" -type f 2>/dev/null | while read -r sofile; do
    ldd "$sofile" 2>/dev/null | awk '/=>/ {print $3}' | while read -r lib; do
        [ -f "$lib" ] || continue
        copy_file "$lib"
    done
done

# Явно копируем tzdb.dat в то место, куда JVM обращается при старте.
# java.home может не совпадать с JAVA_HOME из окружения контейнера.
echo ">>> Ensuring tzdb.dat is reachable"
TZDB_SRC="${JAVA_HOME_PROP}/lib/tzdb.dat"
if [ -f "$TZDB_SRC" ]; then
    # Копируем по пути java.home (уже сделано выше), плюс дублируем
    # по пути ${JAVA_HOME}/jre/lib/tzdb.dat на случай если java.home → JDK,
    # а не JRE (в некоторых дистрибутивах JDK java.home = .../jdk, а не .../jdk/jre)
    for extra_path in \
        "${JAVA_HOME_PROP}/jre/lib" \
        "/usr/jre/lib" \
        "/usr/lib/jvm/jre/lib"; do
        [ "$extra_path" = "${JVM_LIB_DIR}" ] && continue
        mkdir -p "${ROOTFS}${extra_path}"
        cp "$TZDB_SRC" "${ROOTFS}${extra_path}/tzdb.dat"
    done
    echo "  tzdb.dat: OK ($(du -h "$TZDB_SRC" | cut -f1))"
else
    echo "  WARNING: tzdb.dat not found at ${TZDB_SRC}" >&2
    # Поиск по всей FS как last-resort
    TZDB_FOUND=$(find /usr -name "tzdb.dat" 2>/dev/null | head -1)
    if [ -n "$TZDB_FOUND" ]; then
        echo "  Found tzdb.dat at: ${TZDB_FOUND}"
        copy_file "$TZDB_FOUND"
    fi
fi

# ── 3. nc (netcat) ───────────────────────────────────────────────────────────
echo ">>> nc (netcat)"
NC_BIN="$(which nc 2>/dev/null || true)"
if [ -n "$NC_BIN" ]; then
    copy_with_deps "$NC_BIN"
    mkdir -p "${ROOTFS}/usr/bin"
    [ -e "${ROOTFS}/usr/bin/nc" ] || ln -sf "$(readlink -f "$NC_BIN")" "${ROOTFS}/usr/bin/nc"
fi

# ── 4. Нативные библиотеки Ozone ─────────────────────────────────────────────
echo ">>> Native Ozone libs (libhadoop, libisal)"
for sofile in \
    /ozone/share/ozone/lib/native/*.so* \
    /usr/lib64/libhadoop.so* \
    /ozone/share/ozone/lib/native/libisal*.so*; do
    [ -f "$sofile" ] || continue
    copy_file "$sofile"
    ldd "$sofile" 2>/dev/null | awk '/=>/ {print $3}' | while read -r lib; do
        [ -f "$lib" ] || continue
        copy_file "$lib"
    done
done

# ── 5. Kerberos / GSSAPI ─────────────────────────────────────────────────────
# Hadoop использует libgssapi_krb5.so через JNI для Kerberos-аутентификации.
# Копируем все krb5/gssapi .so и их транзитивные зависимости.
echo ">>> Kerberos / GSSAPI libraries"
for krblib in \
    /usr/lib64/libgssapi_krb5.so* \
    /usr/lib64/libkrb5.so* \
    /usr/lib64/libkrb5support.so* \
    /usr/lib64/libk5crypto.so* \
    /usr/lib64/libcom_err.so* \
    /usr/lib64/libgssapi.so* \
    /usr/lib/libgssapi_krb5.so* \
    /usr/lib/libkrb5.so* \
    /usr/lib/libkrb5support.so* \
    /usr/lib/libk5crypto.so* \
    /usr/lib/libcom_err.so* \
    /lib64/libkrb5.so* \
    /lib64/libkrb5support.so* \
    /lib64/libk5crypto.so* \
    /lib64/libcom_err.so*; do
    [ -f "$krblib" ] || continue
    copy_file "$krblib"
    ldd "$krblib" 2>/dev/null | awk '/=>/ {print $3}' | while read -r lib; do
        [ -f "$lib" ] || continue
        copy_file "$lib"
    done
done

# Плагины GSSAPI (механизмы: krb5, spnego и др.)
for gss_dir in /usr/lib64/gss /usr/lib/gss /usr/lib64/krb5/plugins /usr/lib/krb5/plugins; do
    [ -d "$gss_dir" ] || continue
    find "$gss_dir" -name "*.so*" -type f | while read -r sofile; do
        copy_file "$sofile"
        ldd "$sofile" 2>/dev/null | awk '/=>/ {print $3}' | while read -r lib; do
            [ -f "$lib" ] || continue
            copy_file "$lib"
        done
    done
done

# /etc/krb5.conf — placeholder; реальный конфиг монтируется через volume/configmap.
# Без файла krb5-библиотеки выдают предупреждение, но запускаются.
if [ -f /etc/krb5.conf ]; then
    cp /etc/krb5.conf "${ROOTFS}/etc/krb5.conf"
else
    cat > "${ROOTFS}/etc/krb5.conf" <<'EOF'
# Placeholder: примонтируйте реальный krb5.conf через volume или ConfigMap.
[libdefaults]
    default_realm = EXAMPLE.COM
    dns_lookup_realm = false
    dns_lookup_kdc = true
EOF
fi

# ── 6. NSS-библиотеки (DNS, passwd, group) ──────────────────────────────────
echo ">>> NSS libraries"
for nss_so in /lib64/libnss_*.so* /lib/libnss_*.so* /usr/lib64/libnss_*.so* /usr/lib/libnss_*.so*; do
    [ -f "$nss_so" ] || continue
    copy_file "$nss_so"
done

# ── 6. Системные конфигурационные файлы ──────────────────────────────────────
echo ">>> System config"
mkdir -p "${ROOTFS}/etc/ssl/certs" "${ROOTFS}/tmp"

cp /etc/passwd  "${ROOTFS}/etc/"
cp /etc/group   "${ROOTFS}/etc/"
cp /etc/shadow  "${ROOTFS}/etc/" 2>/dev/null || true

# nsswitch.conf: DNS-резолюция и локальные пользователи
if [ -f /etc/nsswitch.conf ]; then
    cp /etc/nsswitch.conf "${ROOTFS}/etc/"
else
    printf 'passwd:\tfiles\ngroup:\tfiles\nhosts:\tdns files\n' > "${ROOTFS}/etc/nsswitch.conf"
fi

# resolv.conf (placeholder — будет смонтирован через Docker)
touch "${ROOTFS}/etc/resolv.conf"
touch "${ROOTFS}/etc/hosts"

# TLS-сертификаты
cp -rL /etc/ssl/certs/. "${ROOTFS}/etc/ssl/certs/" 2>/dev/null || true
cp /etc/pki/tls/certs/ca-bundle.crt "${ROOTFS}/etc/ssl/certs/" 2>/dev/null || true

# ── 7. Временные зоны ────────────────────────────────────────────────────────
echo ">>> Timezone data"
mkdir -p "${ROOTFS}/usr/share"
cp -r /usr/share/zoneinfo "${ROOTFS}/usr/share/" 2>/dev/null || true
[ -f /etc/localtime ] && copy_file /etc/localtime

# ── Итог ─────────────────────────────────────────────────────────────────────
echo ""
echo "=== rootfs size: $(du -sh "${ROOTFS}" | cut -f1) ==="
echo "Done. Rootfs: ${ROOTFS}"
