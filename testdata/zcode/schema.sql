CREATE TABLE schema_migration (
        id text primary key,
        checksum text not null,
        app_version text,
        time_applied integer not null
      );

CREATE TABLE session (
        id text primary key,
        project_id text not null,
        workspace_id text,
        parent_id text,
        slug text not null,
        directory text not null,
        path text,
        title text not null,
        version text not null,
        share_url text,
        summary_additions integer,
        summary_deletions integer,
        summary_files integer,
        summary_diffs text,
        revert text,
        permission text,
        time_created integer not null,
        time_updated integer not null,
        time_compacting integer,
        time_archived integer
      , task_type text not null default 'interactive', title_source text not null default 'first_input'
        check(title_source in ('default', 'first_input', 'generated', 'custom')), title_message_id text, time_title_updated integer, trace_id text);
CREATE INDEX session_project_idx on session(project_id);
CREATE INDEX session_workspace_idx on session(workspace_id);
CREATE INDEX session_parent_idx on session(parent_id);
CREATE INDEX session_task_type_idx on session(task_type);
CREATE INDEX session_trace_idx on session(trace_id);

CREATE TABLE message (
        id text primary key,
        session_id text not null references session(id) on delete cascade,
        time_created integer not null,
        time_updated integer not null,
        data text not null
      , sequence integer);
CREATE INDEX message_session_time_created_id_idx
        on message(session_id, time_created, id);
CREATE INDEX message_session_sequence_idx
        on message(session_id, sequence, time_created, id);
CREATE TRIGGER message_sequence_autofill
      after insert on message
      when new.sequence is null
      begin
        update message
        set sequence = (
          select coalesce(max(sequence), -1) + 1
          from message
          where session_id = new.session_id
        )
        where id = new.id;
      end;

CREATE TABLE part (
        id text primary key,
        message_id text not null references message(id) on delete cascade,
        session_id text not null,
        time_created integer not null,
        time_updated integer not null,
        data text not null
      , sequence integer);
CREATE INDEX part_message_id_id_idx on part(message_id, id);
CREATE INDEX part_session_idx on part(session_id);
CREATE INDEX part_message_sequence_idx
        on part(message_id, sequence, time_created, id);
CREATE INDEX part_session_message_sequence_idx
        on part(session_id, message_id, sequence);
CREATE TRIGGER part_sequence_autofill
      after insert on part
      when new.sequence is null
      begin
        update part
        set sequence = (
          select coalesce(max(sequence), -1) + 1
          from part
          where message_id = new.message_id
        )
        where id = new.id;
      end;

CREATE TABLE todo (
        session_id text not null references session(id) on delete cascade,
        content text not null,
        status text not null,
        priority text not null,
        position integer not null,
        time_created integer not null,
        time_updated integer not null,
        primary key(session_id, position)
      );
CREATE INDEX todo_session_idx on todo(session_id);

CREATE TABLE model_usage (
        id text primary key,
        logical_request_id text not null,
        attempt_index integer not null default 0,
        session_id text not null references session(id) on delete cascade,
        turn_id text,
        trace_id text,
        span_id text,
        assistant_message_id text,
        parent_user_message_id text,
        query_source text not null,
        provider_id text not null,
        model_id text not null,
        variant text,
        agent text,
        mode text,
        task_type text,
        status text not null check(status in ('running', 'completed', 'error', 'cancelled')),
        started_at integer not null,
        first_token_at integer,
        completed_at integer,
        duration_ms integer,
        time_to_first_token_ms integer,
        finish_reason text,
        tool_call_count integer not null default 0,
        input_tokens integer not null default 0,
        output_tokens integer not null default 0,
        reasoning_tokens integer not null default 0,
        cache_creation_input_tokens integer not null default 0,
        cache_read_input_tokens integer not null default 0,
        provider_total_tokens integer,
        computed_total_tokens integer not null default 0,
        retry_count integer not null default 0,
        retryable integer not null default 0 check(retryable in (0, 1)),
        cancelled_by_user integer not null default 0 check(cancelled_by_user in (0, 1)),
        context_exceeded integer not null default 0 check(context_exceeded in (0, 1)),
        error_type text,
        error_code text,
        error_message text,
        raw_usage_json text,
        provider_metadata_json text
      );
CREATE INDEX model_usage_started_model_idx
        on model_usage(started_at, provider_id, model_id);
CREATE INDEX model_usage_session_turn_idx
        on model_usage(session_id, turn_id);
CREATE INDEX model_usage_trace_idx
        on model_usage(trace_id);
CREATE INDEX model_usage_query_source_idx
        on model_usage(query_source);

