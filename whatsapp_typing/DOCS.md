# WhatsApp Typing Sensor

Sensore Home Assistant che segue lo stato **"sta scrivendo…"** di **un singolo contatto**, nella
chat 1-a-1 con te. Niente online/offline, niente ultimo accesso: solo il typing.

Si collega a WhatsApp come **dispositivo collegato** (esattamente come WhatsApp Web) usando
[whatsmeow](https://github.com/tulir/whatsmeow), e pubblica su Home Assistant via **MQTT discovery**.

## Entità create

Con `contact_name: Contatto` ottieni:

| Entità | Cosa fa |
|---|---|
| `binary_sensor.contatto_typing` | ON finché sta scrivendo. Porta gli attributi (media, inizio sessione, motivo di chiusura) |
| `sensor.contatto_status` | `idle` / `typing` / `recording` (nota vocale) / `disconnected` |
| `sensor.contatto_current_duration` | secondi della sessione **in corso**, aggiornato ogni 2 s |
| `sensor.contatto_last_duration` | quanto è durata l'**ultima** sessione |
| `sensor.contatto_last_typing` | timestamp dell'ultima volta che ha scritto |
| `sensor.contatto_sessions_today` | quante volte ha iniziato a scrivere oggi |
| `sensor.contatto_seconds_today` | secondi totali passati a scrivere oggi |

## Come viene calcolata la durata

WhatsApp non manda "ha scritto per 42 secondi": manda solo eventi `composing` (ripetuti ogni
pochi secondi mentre digita) e `paused`. Il servizio li trasforma in sessioni così:

- **`composing`** → sensore ON, parte il cronometro.
- **`paused`** → non spegne subito: aspetta `off_delay` (default **3 s**). Se ricomincia a
  scrivere entro quella finestra è la **stessa** sessione, non due. Serve a non far lampeggiare
  il sensore ogni volta che si ferma a pensare fra una parola e l'altra.
- **Messaggio ricevuto da lei** → sessione chiusa subito (ha premuto invio).
- **`composing_timeout`** (default **20 s**) senza nessun refresh → sessione chiusa comunque.
  È la rete di sicurezza per quando l'evento `paused` si perde: app killata, rete che cade,
  telefono in tasca. Senza questo il sensore resterebbe ON per sempre.

La **durata registrata si ferma sempre sull'ultima prova reale di digitazione** (l'ultimo
`composing` o il `paused`), mai sulla fine del periodo di grazia: i numeri restano onesti.

Se noti che il sensore si spegne mentre lei sta ancora scrivendo, alza `composing_timeout` a 30.
Se invece lampeggia troppo, alza `off_delay` a 5.

## Installazione

Serve HA OS / Supervised e il broker **Mosquitto** (l'add-on ufficiale va benissimo: host e
credenziali vengono presi in automatico dal Supervisor, non devi configurare niente).

1. **Impostazioni → Add-on → Add-on Store → ⋮ (in alto a destra) → Repository**, incolla:
   ```
   https://github.com/hetanfc/haWhatsappBidge
   ```
   e premi **Aggiungi**. Si fa una volta sola.
2. Chiudi, ricarica la pagina: nello store compare la sezione **Home Assistant Add-ons**
   con dentro "WhatsApp Typing Sensor".
3. **Installa**. Alla prima installazione il Supervisor compila l'immagine sul dispositivo:
   qualche minuto su un Raspberry, tu non devi compilare niente a mano.
4. Nella tab **Configurazione**:
   - `phone_number`: il numero del contatto, formato internazionale **solo cifre**, senza `+` e
     senza spazi → `393331234567`
   - `pair_phone`: il **tuo** numero, stesso formato. Se lo compili l'accoppiamento avviene con
     un codice di 8 caratteri invece del QR (nei log un QR ASCII è spesso illeggibile).
   - `contact_name`: `Contatto` → determina i nomi delle entità.
5. Avvia e apri i **Log**. Vedrai:
   ```
   === ACCOPPIAMENTO WHATSAPP ===
   Sul telefono: WhatsApp > Dispositivi collegati > Collega un dispositivo >
   "Collega con numero di telefono", poi digita questo codice:

           ABCD-1234
   ```
   Se hai lasciato `pair_phone` vuoto, trovi invece il QR code da inquadrare.
   Il codice/QR scade dopo pochi minuti: se non fai in tempo, riavvia l'add-on.
   Se la richiesta del codice viene rifiutata da WhatsApp, l'add-on non si ferma: nei log
   compare il QR entro una ventina di secondi e puoi accoppiare con quello.
6. Le entità compaiono da sole in **Impostazioni → Dispositivi → MQTT → WhatsApp Contatto**.

La sessione WhatsApp è salvata in `/data/whatsapp.db` dentro l'add-on: l'accoppiamento si fa
una volta sola, sopravvive a riavvii e aggiornamenti (`/data` non viene toccato quando l'add-on
si aggiorna). Lo perdi solo se **disinstalli** l'add-on: in quel caso rifai il pairing.

### Aggiornamenti

Fai le modifiche, alza `version` in `whatsapp_typing/config.yaml`, committa e pusha. Entro
qualche ora HA se ne accorge da solo; per vederlo subito: **Add-on Store → ⋮ → Controlla
aggiornamenti**, poi compare il pulsante **Aggiorna** sull'add-on. Se non alzi la versione,
HA non vede niente: è il numero di versione a fare da trigger, non il commit.

### Senza broker MQTT

Metti `publisher: ha` nelle opzioni: il servizio scrive direttamente sull'API di Home Assistant
attraverso il Supervisor (nessun token da configurare). Limite: le entità create via API
esistono solo finché il servizio le aggiorna e non vengono ripristinate al riavvio di HA finché
non arriva il primo aggiornamento. Con MQTT è tutto più solido.

## Installazione con Docker (alternativa, fuori da HA OS)

Dalla cartella `whatsapp_typing/`:

```bash
cp .env.example .env   # compila WT_PHONE, WT_PAIR_PHONE, MQTT_HOST
docker compose up -d --build
docker compose logs -f
```

Il primo avvio mostra codice o QR nei log. Il DB della sessione finisce in `./data`.

## Configurazione

| Opzione add-on | Env var | Default | Note |
|---|---|---|---|
| `phone_number` | `WT_PHONE` | — | Numero del contatto, solo cifre con prefisso |
| `pair_phone` | `WT_PAIR_PHONE` | vuoto | Il tuo numero: accoppiamento con codice invece del QR |
| `contact_name` | `WT_NAME` | `Contatto` | Base dei nomi entità |
| `composing_timeout` | `WT_COMPOSING_TIMEOUT` | `20` | Secondi senza refresh prima di spegnere |
| `off_delay` | `WT_OFF_DELAY` | `3` | Grazia dopo `paused` (0 = spegni subito) |
| `mark_online` | `WT_MARK_ONLINE` | `true` | Vedi sotto |
| `publisher` | `WT_PUBLISHER` | `mqtt` | `mqtt` o `ha` |
| `jid_override` | `WT_JID` | vuoto | Via di fuga se il contatto arriva su un JID `@lid` |
| `push_name` | `WT_PUSH_NAME` | `Home Assistant` | Solo se WhatsApp non ha ancora sincronizzato il tuo nome |
| `log_level` | `WT_LOG_LEVEL` | `info` | `debug` mostra ogni evento di presence |
| `mqtt_*` | `MQTT_HOST`, `MQTT_PORT`, `MQTT_USER`, `MQTT_PASSWORD`, `MQTT_TLS` | auto | Nell'add-on vengono dal Supervisor |

## Cose da sapere prima di usarlo

- **`mark_online` ti fa risultare online.** WhatsApp manda gli aggiornamenti di presence (typing
  incluso) solo a chi si dichiara "available": è la stessa cosa che fa WhatsApp Web quando lo
  tieni aperto. Effetto collaterale noto: finché un client è attivo, il telefono può **smettere
  di mostrarti le notifiche push** (le considera già "viste" da un altro dispositivo). Con
  `mark_online: false` non ti dichiari online, ma è molto probabile che gli eventi di typing
  non arrivino più: è un compromesso da verificare sul campo, non c'è una terza strada.
- **È un client non ufficiale.** Uso passivo, nessun messaggio inviato, ma il rischio di ban di
  WhatsApp non è formalmente zero.
- Occupa uno dei **4 slot dispositivi collegati** del tuo account.
- Il typing lo vedi solo se lei **ha la chat con te aperta e sta digitando**: WhatsApp non manda
  eventi per chat che non ti riguardano.
- Ovviamente registri dati sul comportamento di un'altra persona in una cronologia consultabile:
  sono le stesse informazioni che WhatsApp ti mostra già, ma conservate nel tempo. Regolati.

## Se il sensore non si accende mai

1. Metti `log_level: debug` e riavvia.
2. Scrivile qualcosa e falla rispondere: nei log deve comparire
   `chat presence state=composing`.
3. Se invece vedi `chat presence from another chat (ignored) jid=...@lid`, WhatsApp sta usando
   l'indirizzamento LID e la risoluzione automatica non ha funzionato: copia quel JID in
   `jid_override` e riavvia.
4. Se non compare **niente**: `subscribe presence failed` nei log significa che la sottoscrizione
   non è passata (di solito `mark_online: false`, oppure push name mancante — viene impostato
   da solo dopo il primo avvio).

## Esempi Home Assistant

Notifica se sta scrivendo da più di un minuto (ci sta pensando parecchio):

```yaml
automation:
  - alias: Contatto scrive da un minuto
    triggers:
      - trigger: numeric_state
        entity_id: sensor.contatto_current_duration
        above: 60
    actions:
      - action: notify.mobile_app_telefono
        data:
          message: "Contatto sta scrivendo da oltre un minuto…"
```

Statistica giornaliera leggibile in una card:

```yaml
template:
  - sensor:
      - name: Contatto tempo scritto oggi
        state: >
          {{ (states('sensor.contatto_seconds_today') | int(0) / 60) | round(1) }}
        unit_of_measurement: min
```

Storico: `binary_sensor.contatto_typing` finisce già nella history/logbook, e
`sensor.contatto_seconds_today` ha `state_class: total_increasing`, quindi puoi metterlo nelle
statistiche a lungo termine.

## Sviluppo

```bash
docker run --rm -v "$PWD":/src -w /src golang:1.25-alpine go test ./...
```

I test coprono la macchina a stati (grazia, timeout, chiusura su messaggio, totali giornalieri)
senza bisogno di WhatsApp.

| File | Ruolo |
|---|---|
| `main.go` | avvio e spegnimento pulito |
| `config.go` | configurazione da env var |
| `whatsapp.go` | client whatsmeow, login, sottoscrizione presence, filtro sulla chat |
| `tracker.go` | macchina a stati typing → sessioni |
| `publisher.go` | definizione delle entità + publisher REST |
| `mqtt.go` | MQTT discovery e pubblicazione stato |
