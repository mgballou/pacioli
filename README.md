# pacioli

A double-entry ledger service in Go, named for Luca Pacioli, who published the
first written account of double-entry bookkeeping in Venice in 1494.

Amounts are signed minor units — debit positive, credit negative — so asking
whether a transaction balances is one sum against zero. Postgres holds that
rule in a deferred constraint trigger, so no caller can post an entry that
does not balance.

## Tests

    make test

Needs Go 1.26 and a container runtime. The Makefile starts Postgres itself.
