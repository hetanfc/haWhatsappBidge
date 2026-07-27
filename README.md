# Home Assistant Add-ons

Repository di add-on per Home Assistant.

## Installazione

**Impostazioni → Add-on → Add-on Store → ⋮ → Repository**, incolla questo URL e premi Aggiungi:

```
https://github.com/hetanfc/haWhatsappBidge
```

## Add-on disponibili

### [WhatsApp Typing Sensor](whatsapp_typing/)

Sensore che segue lo stato **"sta scrivendo…"** di un singolo contatto WhatsApp nella chat 1-a-1
con te, con durata realistica della sessione e totali giornalieri. Si collega come dispositivo
collegato (come WhatsApp Web) e pubblica su HA via MQTT discovery.

Documentazione completa: [whatsapp_typing/DOCS.md](whatsapp_typing/DOCS.md)

## Aggiornare un add-on

Modifica il codice, alza `version` in `whatsapp_typing/config.yaml`, `git push`. In HA:
**Add-on Store → ⋮ → Controlla aggiornamenti** e comparirà il pulsante Aggiorna.
