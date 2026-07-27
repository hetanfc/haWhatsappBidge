# Changelog

## 1.0.1

- Aggiunto `build.yaml`: senza quel file il Supervisor non passa `BUILD_FROM` e la build finiva
  sulla base image amd64 anche su dispositivi aarch64/armv7, fallendo con
  "no match for platform in manifest"
- Il Dockerfile non ha più un `BUILD_FROM` di default: meglio fallire subito che costruire
  per l'architettura sbagliata

## 1.0.0

Prima versione.

- Sensore `binary_sensor.<contatto>_typing` con lo stato "sta scrivendo" di un singolo contatto
- Durata della sessione in corso, durata dell'ultima, timestamp dell'ultima volta, totali giornalieri
- Distinzione fra testo e registrazione di una nota vocale
- Debounce configurabile (`off_delay`) e timeout di sicurezza (`composing_timeout`)
- Accoppiamento con codice a 8 caratteri (`pair_phone`) oppure QR code
- Pubblicazione via MQTT discovery, con fallback sull'API di Home Assistant (`publisher: ha`)
