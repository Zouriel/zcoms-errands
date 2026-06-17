# zcoms-errands

The **errands** component for [zcoms](https://github.com/Zouriel/zcoms). A pure-Go
process that dispatches and drives autonomous **interviewer→producer** agents at a
contact: it asks them what's needed one question at a time, then builds a
deliverable from their answers and reports back to you.

It owns no Telegram session — the core daemon does. It reaches Telegram over the
daemon's IPC (subscribe for the contact's replies, send/sendfile to message them)
and WhatsApp over the Baileys sidecar. Errand commands arrive on `errands.sock`
(used by `zc errand …` and the bridge `errand …` command).

## Install
```sh
zc install errands     # downloads the prebuilt binary + sets up the service
zc errand start <@user|wa:NUMBER|#index> <brief>
```
