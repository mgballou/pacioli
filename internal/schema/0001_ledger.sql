-- The ledger schema: accounts, transactions, postings and idempotency keys.
-- Applied verbatim by internal/schema. There is no migration framework.

-- Error codes raised below. Postgres leaves these classes free for callers.
--
--   LB001  transaction does not balance (or has no postings)
--   LB002  attempt to change an append-only row
--   LB003  an idempotency key was committed without the transaction it names
--   LB004  a settled idempotency record was changed

CREATE TYPE account_kind AS ENUM (
    'asset',
    'liability',
    'equity',
    'revenue',
    'expense'
);

CREATE TABLE accounts (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    code       text NOT NULL UNIQUE CHECK (code ~ '^[a-z][a-z0-9_.]*$'),
    name       text NOT NULL CHECK (btrim(name) <> ''),
    kind       account_kind NOT NULL,
    currency   text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    created_at timestamptz NOT NULL DEFAULT now(),

    -- Redundant given the primary key, and what lets postings name the currency.
    UNIQUE (id, currency)
);

CREATE TABLE transactions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    description text NOT NULL CHECK (btrim(description) <> ''),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- Same trick as accounts, for the other half of the currency check.
    UNIQUE (id, currency)
);

CREATE TABLE postings (
    -- Identity rather than uuid: the log wants an order.
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id uuid   NOT NULL,
    account_id     uuid   NOT NULL,
    currency       text   NOT NULL,

    -- Minor units, never a float. A zero-value posting says nothing.
    amount_minor bigint NOT NULL CHECK (amount_minor <> 0),
    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Both keys carry currency, so a cross-currency posting fails as a foreign key.
    FOREIGN KEY (transaction_id, currency) REFERENCES transactions (id, currency),
    FOREIGN KEY (account_id, currency) REFERENCES accounts (id, currency)
);

CREATE INDEX postings_transaction_id_idx ON postings (transaction_id);
CREATE INDEX postings_account_id_idx ON postings (account_id, id);

-- The constraint the ledger exists for, deferred to the end of the transaction so
-- the postings of one entry can arrive across several statements.
CREATE FUNCTION assert_transaction_balances(txn_id uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    legs bigint;
    net  bigint;
BEGIN
    SELECT count(*), coalesce(sum(amount_minor), 0)
      INTO legs, net
      FROM postings
     WHERE transaction_id = txn_id;

    IF legs = 0 THEN
        RAISE EXCEPTION 'transaction % has no postings', txn_id
            USING ERRCODE = 'LB001',
                  HINT = 'A transaction needs at least two postings summing to zero.';
    END IF;

    IF net <> 0 THEN
        RAISE EXCEPTION 'transaction % does not balance: % postings net to % (want 0)',
            txn_id, legs, net
            USING ERRCODE = 'LB001',
                  HINT = 'Debits are positive, credits negative; the sides must cancel.';
    END IF;
END;
$$;

-- Two triggers, because there are two ways to end up unbalanced, and two functions
-- because plpgsql resolves NEW's fields when the function is planned.
CREATE FUNCTION transactions_balance_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_transaction_balances(NEW.id);
    RETURN NULL;
END;
$$;

CREATE FUNCTION postings_balance_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM assert_transaction_balances(NEW.transaction_id);
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER transactions_must_balance
    AFTER INSERT ON transactions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION transactions_balance_check();

CREATE CONSTRAINT TRIGGER postings_must_balance
    AFTER INSERT ON postings
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION postings_balance_check();

-- Append-only. Without this the balance check above is theatre.
CREATE FUNCTION reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION '% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'LB002',
              HINT = 'Correct a mistake with a reversing transaction.';
END;
$$;

CREATE TRIGGER transactions_are_append_only
    BEFORE UPDATE OR DELETE ON transactions
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER postings_are_append_only
    BEFORE UPDATE OR DELETE ON postings
    FOR EACH ROW EXECUTE FUNCTION reject_mutation();

-- TRUNCATE takes no row locks and fires no row triggers.
CREATE TRIGGER transactions_are_never_truncated
    BEFORE TRUNCATE ON transactions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER postings_are_never_truncated
    BEFORE TRUNCATE ON postings
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- A client's claim that a write should happen at most once. The primary key is the
-- whole mechanism: a second inserter blocks until the first commits or rolls back,
-- and then either finds the key taken or takes it.
CREATE TABLE idempotency_keys (
    -- Chosen by the client, so the shape is checked and the content is not.
    key text PRIMARY KEY
        CONSTRAINT idempotency_keys_key_shape CHECK (key ~ '^[[:graph:]]{16,255}$'),

    -- What the key was first used for. A digest, because this table only compares.
    request_hash bytea NOT NULL CHECK (length(request_hash) = 32),

    -- The result. Nullable for as long as one transaction takes, the row being
    -- inserted to reserve the key before the work is done. A committed row has one.
    transaction_id uuid REFERENCES transactions (id),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- Reserved and never settled cannot be committed. Deferred for the same reason the
-- balance triggers are. The row is read back rather than taken from NEW, because a
-- deferred trigger is handed the tuple as it stood when the event fired.
CREATE FUNCTION idempotency_key_names_a_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    settled uuid;
    found_it boolean;
BEGIN
    SELECT transaction_id, true INTO settled, found_it
      FROM idempotency_keys
     WHERE key = NEW.key;

    -- Reserved and then deleted in the same transaction is a reservation given up.
    IF found_it IS NULL THEN
        RETURN NULL;
    END IF;

    IF settled IS NULL THEN
        RAISE EXCEPTION 'idempotency key % was committed without a transaction', NEW.key
            USING ERRCODE = 'LB003',
                  HINT = 'Reserve the key, do the work, then record the transaction it created.';
    END IF;
    RETURN NULL;
END;
$$;

CREATE CONSTRAINT TRIGGER idempotency_keys_must_be_settled
    AFTER INSERT OR UPDATE ON idempotency_keys
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION idempotency_key_names_a_transaction();

-- Settled once, and then it is the answer: the stored result has to be the first
-- result. DELETE is deliberately allowed, because these rows expire one day.
CREATE FUNCTION idempotency_record_is_final() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.transaction_id IS NOT NULL OR NEW.key <> OLD.key OR NEW.request_hash <> OLD.request_hash THEN
        RAISE EXCEPTION 'idempotency key % is already settled', OLD.key
            USING ERRCODE = 'LB004',
                  HINT = 'The first result under a key is the only result. Use a different key.';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER idempotency_keys_settle_once
    BEFORE UPDATE ON idempotency_keys
    FOR EACH ROW EXECUTE FUNCTION idempotency_record_is_final();

-- Balances are derived, never stored, and signed the same way as a posting, so an
-- asset account with money in it reads positive and a liability reads negative.
CREATE VIEW account_balances AS
    SELECT a.id AS account_id,
           a.code,
           a.name,
           a.kind,
           a.currency,
           coalesce(sum(p.amount_minor), 0) AS balance_minor,
           count(p.id)                      AS posting_count
      FROM accounts a
      LEFT JOIN postings p ON p.account_id = a.id
     GROUP BY a.id;
