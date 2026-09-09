DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS inbox_messages;
DROP FUNCTION IF EXISTS reject_outbox_payload_mutation();
