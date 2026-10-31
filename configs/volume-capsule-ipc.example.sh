#!/bin/sh
# Example volume program for marusya-agent -volume_cmd
# (MARUSYA_VOLUME_CMD). It adapts capsule-ipc, the vendor IPC client that the
# VK Capsule Mini panel uses, to the generic contract:
#
#   get          -> "<level> <state>"
#   set <0..100> -> set the level
#   mute on|off  -> mute or unmute
#
# Install as /usr/local/bin/marusya-volume, chmod 755, then set
# MARUSYA_VOLUME_CMD=/usr/local/bin/marusya-volume.
#
# Why not amixer: on this speaker the panel and AirPlay write the level
# through the same IPC, so changing only the ALSA control makes the panel
# and the phone slider disagree. capsule-ipc goes through that IPC.
set -eu

IPC=${CAPSULE_IPC:-/usr/local/bin/capsule-ipc}

case "${1:-}" in
get)
    # capsule-ipc GET replies "OK <level> <muted> ...".
    set -- $("$IPC" GET)
    [ "${1:-}" = OK ] || { echo "$IPC GET: unexpected reply" >&2; exit 1; }
    level=${2:?}
    if [ "${3:-0}" = 1 ]; then state=muted; else state=unmuted; fi
    printf '%s %s\n' "$level" "$state"
    ;;
set)
    level=${2:?}
    "$IPC" "VOL $level" >/dev/null
    ;;
mute)
    want=${2:?}
    set -- $("$IPC" GET)
    muted=${3:-0}
    case "$want:$muted" in
    on:1 | off:0) exit 0 ;;
    on:0 | off:1) "$IPC" MUTE >/dev/null ;;
    *) echo "$0 mute: expected on or off, got $want" >&2; exit 2 ;;
    esac
    ;;
*)
    echo "usage: $0 get | set <0..100> | mute on|off" >&2
    exit 2
    ;;
esac
