# Changelog

## 1.7.0

- Aggiunto il relay MQTT per chiamare Gianni con `@gianni` dalla chat configurata.
- Le richieste possono partire sia dal proprietario sia dal contatto.
- Aggiunto l'invio WhatsApp di risposte testuali, posizioni e immagini.
- Le risposte del bot vengono riconosciute e non generano loop.

## 1.6.0

- Basta "Non disponibile" a ogni riavvio: l'availability resta solo su una nuova entità
  dedicata, `binary_sensor.<contatto>_bridge`. Tutte le altre conservano l'ultimo valore
- Nuova entità `event.<contatto>_eventi`, pensata per far scattare le notifiche: emette un
  evento a ogni cosa che succede, anche due identiche di fila, con `event_type` e `label`
- Nuovo `sensor.<contatto>_last_event` con l'ultimo evento in chiaro, per chi usa `publisher: ha`
- L'ultimo evento sopravvive al riavvio, ma il contatore no: nessuna notifica di eventi vecchi
  quando l'add-on riparte

## 1.5.0

- Lo stato conosciuto sopravvive ai riavvii: le entità non tornano più a "Sconosciuto" e gli
  eventi riproposti da WhatsApp alla riconnessione vengono riconosciuti come già visti
- Corretta la reazione ripetuta: il controllo anti-replay confrontava l'etichetta con l'ultimo
  evento di qualsiasi tipo, quindi bastava un evento qualunque in mezzo per far ricomparire la
  stessa reazione
- Le disconnessioni brevi non segnano più tutto "Non disponibile": nuova opzione
  `availability_grace` (default 120 s) prima di dichiarare le entità non disponibili
- Una reazione rimossa ora si legge `nessuna` invece di `unknown`: "sconosciuto" significa che
  non lo sappiamo, non che l'ha tolta
- I contatori giornalieri sopravvivono a riavvii e aggiornamenti, ma solo entro la stessa giornata

## 1.4.2

- Aggiunte icona e logo dedicati per lo Store di Home Assistant

## 1.4.1

- Corretto il manifest YAML della versione 1.4.0, che veniva scartato dal Supervisor

## 1.4.0

- Nuovo archivio SQLite limitato alla chat configurata: conserva ID, tipo, testo/didascalia,
  direzione, risposta citata e flag del messaggio per 60 giorni (configurabile)
- Reazioni complete nella timeline: aggiunta, cambio e rimozione dell'emoji, con indicazione
  del messaggio/foto/video/vocale a cui si riferiscono
- Modifiche con confronto tra testo precedente e nuovo; eliminazioni con recupero del testo o
  della didascalia originale quando il messaggio era già stato osservato
- Le risposte citate indicano il messaggio bersaglio; vengono segnalati anche inoltrati,
  effimeri e contenuti a visualizzazione singola
- Presenza `online`/`offline`, ultimo accesso quando WhatsApp lo espone e relativo cambio di
  presenza nella timeline
- Conteggio di pause e riprese durante le sessioni di scrittura, con riepilogo leggibile
- Nuove entità per presenza, reazioni, modifiche, eliminazioni, pause e riprese
- Protezione dai replay fuori ordine dopo una riconnessione e limite sicuro per lo stato HA
- Nuove opzioni `store_message_content` e `message_retention_days`

## 1.3.2

- Corretto un falso positivo nelle ricevute di lettura: gli eventi generati dal telefono o
  da altri dispositivi propri non vengono più attribuiti al contatto configurato
- Aggiunto un test per distinguere le ricevute del contatto da `read` e `read-self` propri

## 1.3.1

- Mantiene la presenza necessaria agli eventi `typing`, ma invia ricevute di consegna
  inattive: il dispositivo collegato si comporta come WhatsApp Web in background e non
  dovrebbe sopprimere le notifiche sul telefono

## 1.3.0

- Le ricevute dicono **a cosa si riferiscono**: "ha letto (foto delle 17:02)",
  "ha ascoltato (videomessaggio delle 21:14)"
- Le riproduzioni ripetute vengono contate: "ha riguardato videomessaggio delle 21:14 (3ª volta)"
- Due nuove entità: `last_read_target` e `last_played_target`
- I messaggi inviati vengono ricordati in `/data` con **solo id, orario e tipo**, mai il
  contenuto, e cancellati dopo 60 giorni
- Per i messaggi mandati prima dell'installazione l'etichetta resta generica: quegli id non
  sono nella tabella

## 1.2.0

- Nuovo sensore unificato `activity`: un'unica entità con lo stato corrente (inattivo,
  sta scrivendo, registra vocale, messaggio ricevuto, ha letto, ha ascoltato, consegnato),
  così Cronologia e Registro di Home Assistant diventano leggibili riga per riga
- Attributo `timeline` con le ultime 50 voci già formattate, per una card markdown
- Nuova opzione `activity_sticky` (default 30 s): quanto resta visibile un evento istantaneo
  prima di tornare a inattivo
- Mentre sta scrivendo, lo stato resta "sta scrivendo": le ricevute non lo sovrascrivono,
  ma finiscono comunque nella cronologia
- La cronologia viene pubblicata su un topic dedicato solo quando cambia, e il publisher `ha`
  non riscrive più entità identiche: molte meno righe nel database di Home Assistant

## 1.1.0

- Nuovi sensori dalle ricevute di ritorno: `last_read` (spunte blu), `last_delivered`
  (consegnato), `last_played` (vocale o videomessaggio riprodotto) e `reads_today`
- Nuovi sensori sui messaggi in arrivo: `last_message` e `messages_today`, con l'attributo
  `last_message_type` (testo, foto, video, videomessaggio, vocale, sticker, documento…)
- Del contenuto dei messaggi non viene letto né pubblicato niente: solo orario e tipo
- Le ricevute riproposte dopo una riconnessione non fanno più tornare indietro i timestamp
  né gonfiano i contatori

## 1.0.2

- Corretto l'accoppiamento con codice: WhatsApp valida il nome del client e rifiuta con
  `400 bad-request` tutto ciò che non è un `Browser (OS)` comune. Veniva mandato
  "Home Assistant (Linux)", ora "Chrome (Linux)"
- La richiesta del codice aspetta che l'handshake di login sia completo, come richiesto da
  whatsmeow, invece di partire subito dopo la connessione
- Se l'accoppiamento con codice fallisce l'add-on non muore più: ripiega sul QR code, che
  compare nei log entro una ventina di secondi
- La versione mostrata nei log e nel dispositivo MQTT è quella vera dell'add-on

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
