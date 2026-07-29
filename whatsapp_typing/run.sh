#!/usr/bin/with-contenv bashio
# Maps the add-on options onto the environment variables the binary reads.
set -e

export WT_PHONE="$(bashio::config 'phone_number')"
export WT_NAME="$(bashio::config 'contact_name')"
export WT_COMPOSING_TIMEOUT="$(bashio::config 'composing_timeout')"
export WT_OFF_DELAY="$(bashio::config 'off_delay')"
export WT_ACTIVITY_STICKY="$(bashio::config 'activity_sticky')"
export WT_AVAILABILITY_GRACE="$(bashio::config 'availability_grace')"
export WT_MARK_ONLINE="$(bashio::config 'mark_online')"
export WT_STORE_MESSAGE_CONTENT="$(bashio::config 'store_message_content')"
export WT_MESSAGE_RETENTION_DAYS="$(bashio::config 'message_retention_days')"
export WT_LOG_LEVEL="$(bashio::config 'log_level')"
export WT_PUBLISHER="$(bashio::config 'publisher')"
export WT_GIANNI_ENABLED="$(bashio::config 'gianni_enabled')"
export WT_GIANNI_MENTION="$(bashio::config 'gianni_mention')"
export WT_GIANNA_MENTION="$(bashio::config 'gianna_mention')"
export WT_GIANNI_TOPIC_IN="$(bashio::config 'gianni_topic_in')"
export WT_GIANNI_TOPIC_OUT="$(bashio::config 'gianni_topic_out')"
export WT_GIANNI_TOPIC_STATUS="$(bashio::config 'gianni_topic_status')"
export WT_GIANNI_ACK_ENABLED="$(bashio::config 'gianni_ack_enabled')"
export WT_GIANNI_IMAGE_MAX_MB="$(bashio::config 'gianni_image_max_mb')"
export WT_DB_PATH="/data/whatsapp.db"

if bashio::config.has_value 'pair_phone'; then
    export WT_PAIR_PHONE="$(bashio::config 'pair_phone')"
fi
if bashio::config.has_value 'jid_override'; then
    export WT_JID="$(bashio::config 'jid_override')"
fi
if bashio::config.has_value 'push_name'; then
    export WT_PUSH_NAME="$(bashio::config 'push_name')"
fi

if [ "${WT_PUBLISHER}" = "ha" ]; then
    # Talk to the core through the Supervisor proxy, no token to configure.
    export HA_URL="http://supervisor/core"
    export HA_TOKEN="${SUPERVISOR_TOKEN}"
    bashio::log.info "Pubblicazione via API Home Assistant (nessun broker MQTT)"
elif bashio::config.has_value 'mqtt_host'; then
    export MQTT_HOST="$(bashio::config 'mqtt_host')"
    export MQTT_PORT="$(bashio::config 'mqtt_port')"
    export MQTT_USER="$(bashio::config 'mqtt_user')"
    export MQTT_PASSWORD="$(bashio::config 'mqtt_password')"
    export MQTT_TLS="$(bashio::config 'mqtt_tls')"
    bashio::log.info "Broker MQTT configurato a mano: ${MQTT_HOST}:${MQTT_PORT}"
elif bashio::services.available "mqtt"; then
    export MQTT_HOST="$(bashio::services mqtt 'host')"
    export MQTT_PORT="$(bashio::services mqtt 'port')"
    export MQTT_USER="$(bashio::services mqtt 'username')"
    export MQTT_PASSWORD="$(bashio::services mqtt 'password')"
    export MQTT_TLS="$(bashio::services mqtt 'ssl')"
    bashio::log.info "Broker MQTT preso dal Supervisor: ${MQTT_HOST}:${MQTT_PORT}"
else
    bashio::exit.nok "Nessun broker MQTT trovato: installa il add-on Mosquitto, compila mqtt_host, oppure metti publisher: ha"
fi

if bashio::var.is_empty "${WT_PHONE}" && bashio::var.is_empty "${WT_JID:-}"; then
    bashio::exit.nok "Imposta phone_number (numero internazionale senza + e senza spazi, es. 393331234567)"
fi

exec /usr/bin/whatsapp-typing
