-- +goose Up
-- Project chat messages from the event journal. Match a send_message result
-- to the current model reply because call IDs can repeat in later turns.
CREATE FUNCTION run_assistant_messages(run_key TEXT, first_seq INTEGER DEFAULT 0)
RETURNS TABLE (seq INTEGER, body TEXT)
LANGUAGE SQL STABLE AS $$
    SELECT e.seq, e.event->>'text'
    FROM run_events e
    WHERE e.run_id = run_key AND e.seq >= first_seq
      AND (e.event->>'text') ~ '[^[:space:]]'
      AND (
        e.event->>'kind' = 'model_reply'
        OR (
          e.event->>'kind' = 'tool_result'
          AND e.event->'is_error' IS DISTINCT FROM 'true'::jsonb
          AND EXISTS (
            SELECT 1
            FROM (
              SELECT previous.event->'tool_calls' AS calls
              FROM run_events previous
              WHERE previous.run_id = e.run_id AND previous.seq < e.seq
                AND previous.event->>'kind' = 'model_reply'
              ORDER BY previous.seq DESC LIMIT 1
            ) reply
            CROSS JOIN LATERAL jsonb_array_elements(reply.calls) call
            WHERE call->>'id' = e.event->>'call_id'
            HAVING count(*) = 1 AND bool_and(call->>'name' = 'send_message')
          )
        )
      )
$$;

-- +goose Down
DROP FUNCTION run_assistant_messages(TEXT, INTEGER);
