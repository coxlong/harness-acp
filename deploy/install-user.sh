#!/bin/sh
set -eu

if [ "$#" -gt 1 ]; then
    printf 'Usage: %s [path-to-harnessd]\n' "$0" >&2
    exit 2
fi

binary=${1:-./harnessd}
if [ ! -f "$binary" ] || [ ! -x "$binary" ]; then
    printf 'Executable not found: %s\n' "$binary" >&2
    exit 1
fi

if [ "$(id -u)" -eq 0 ]; then
    printf 'Run this installer as the account that will use harnessd, without sudo.\n' >&2
    exit 1
fi

if ! systemctl --user show-environment >/dev/null; then
    printf 'Could not connect to the current user’s systemd manager.\n' >&2
    exit 1
fi

deploy_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
config_home=${XDG_CONFIG_HOME:-"$HOME/.config"}
config_dir=$config_home/harnessd
unit_dir=$config_home/systemd/user
bin_dir=$HOME/.local/bin
token_file=$config_dir/token.env

umask 077
mkdir -p -- "$config_dir" "$unit_dir" "$bin_dir"

if [ ! -e "$token_file" ]; then
    secret=$(command -p od -An -v -tx1 -N 32 /dev/urandom | tr -d ' \n')
    if [ "${#secret}" -ne 64 ]; then
        printf 'Could not generate a harnessd access token.\n' >&2
        exit 1
    fi
    candidate=$(mktemp "$config_dir/.token.env.XXXXXX")
    trap 'rm -f -- "$candidate"' EXIT
    trap 'exit 1' HUP INT TERM
    printf 'HARNESSD_TOKEN=%s\n' "$secret" > "$candidate"
    if ! ln -- "$candidate" "$token_file" 2>/dev/null && [ ! -f "$token_file" ]; then
        printf 'Could not save the harnessd access token at %s.\n' "$token_file" >&2
        exit 1
    fi
    rm -f -- "$candidate"
    trap - EXIT HUP INT TERM
    unset secret
fi

if [ -L "$token_file" ] || [ ! -f "$token_file" ] ||
    [ "$(wc -l < "$token_file")" -ne 1 ] ||
    ! LC_ALL=C grep -Eq '^HARNESSD_TOKEN=[0-9a-f]{64}$' "$token_file"; then
    printf 'Invalid managed token file: %s\n' "$token_file" >&2
    exit 1
fi
chmod 0600 -- "$token_file"

replace_file() {
    candidate=$(mktemp "$2.XXXXXX")
    if ! install -m "$3" -- "$1" "$candidate" || ! mv -fT -- "$candidate" "$2"; then
        rm -f -- "$candidate"
        return 1
    fi
}

if [ ! "$binary" -ef "$bin_dir/harnessd" ]; then
    replace_file "$binary" "$bin_dir/harnessd" 0755
fi
replace_file "$deploy_dir/harnessd.service" "$unit_dir/harnessd.service" 0644

systemctl --user daemon-reload
systemctl --user enable harnessd.service
systemctl --user restart harnessd.service
printf 'harnessd user service installed. Access token file: %s\n' "$token_file"
