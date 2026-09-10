-- Reverter remove a contabilidade paralela e não toca no ledger do §6.4.
DROP TABLE IF EXISTS ledger_postings;
DROP FUNCTION IF EXISTS assert_postings_balanced();
DROP FUNCTION IF EXISTS reject_posting_mutation();
