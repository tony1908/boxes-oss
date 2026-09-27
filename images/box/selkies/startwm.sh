#!/usr/bin/env bash

if which nvidia-smi >/dev/null 2>&1 && ls -A /dev/dri 2>/dev/null && [ "${DISABLE_ZINK}" = "false" ]; then
    export LIBGL_KOPPER_DRI2=1
    export MESA_LOADER_DRIVER_OVERRIDE=zink
    export GALLIUM_DRIVER=zink
fi

exec dbus-launch --exit-with-session /usr/bin/xfce4-session >/dev/null 2>&1
