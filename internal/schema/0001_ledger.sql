-- The ledger schema: accounts, transactions, postings and idempotency keys.
-- Applied verbatim by internal/schema. There is no migration framework.


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

    -- Lets a posting name (account_id, currency) as a foreign key.
    UNIQUE (id, currency)
);

CREATE TABLE transactions (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    currency    text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    description text NOT NULL CHECK (btrim(description) <> ''),
    occurred_at timestamptz NOT NULL DEFAULT now(),
    created_at  timestamptz NOT NULL DEFAULT now(),

    -- The same, for the transaction side of that check.
    UNIQUE (id, currency)
);

CREATE TABLE postings (
    id             bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id uuid   NOT NULL,
    account_id     uuid   NOT NULL,
    currency       text   NOT NULL,

    -- Signed minor units, never a float. Zero is refused.
    amount_minor bigint NOT NULL CHECK (amount_minor <> 0),
    created_at   timestamptz NOT NULL DEFAULT now(),

    -- Both keys carry currency, so a posting cannot cross currencies.
    FOREIGN KEY (transaction_id, currency) REFERENCES transactions (id, currency),
    FOREIGN KEY (account_id, currency) REFERENCES accounts (id, currency)
);

CREATE INDEX postings_transaction_id_idx ON postings (transaction_id);
CREATE INDEX postings_account_id_idx ON postings (account_id, id);

-- A transaction must balance. Deferred to commit; SET CONSTRAINTS ALL IMMEDIATE asks early.
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

-- Two functions because plpgsql resolves NEW's fields when the function is planned.
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

-- Append-only: without this, a committed posting could be deleted.
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

-- TRUNCATE fires no row triggers, so it needs a trigger of its own.
CREATE TRIGGER transactions_are_never_truncated
    BEFORE TRUNCATE ON transactions
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

CREATE TRIGGER postings_are_never_truncated
    BEFORE TRUNCATE ON postings
    FOR EACH STATEMENT EXECUTE FUNCTION reject_mutation();

-- The primary key is the retry contract: a duplicate blocks rather than posts twice.
CREATE TABLE idempotency_keys (
    -- Chosen by the client, so the shape is checked and the content is not.
    key text PRIMARY KEY
        CONSTRAINT idempotency_keys_key_shape CHECK (key ~ '^[[:graph:]]{16,255}$'),

    -- A digest of the request the key was first used for. A different one is refused.
    request_hash bytea NOT NULL CHECK (length(request_hash) = 32),

    -- Null only between reserving the key and recording the result.
    transaction_id uuid REFERENCES transactions (id),

    created_at timestamptz NOT NULL DEFAULT now()
);

-- A committed key must name a transaction. Deferred, and it reads the row back, not NEW.
CREATE FUNCTION idempotency_key_names_a_transaction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    settled uuid;
    found_it boolean;
BEGIN
    SELECT transaction_id, true INTO settled, found_it
      FROM idempotency_keys
     WHERE key = NEW.key;

    -- Reserved and then deleted is a reservation given up.
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

-- A settled key is final: the first result is the only result. DELETE stays allowed.
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

-- Balances are derived, never stored, and signed like a posting.
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
