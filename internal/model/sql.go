package model

import (
	"fmt"
	"regexp"
)

// bigintLiteral matches a non-negative bigint. Unique SQL IDs are validated
// against it before being interpolated into an anonymous block, so a malformed
// or hostile value can never inject SQL.
var bigintLiteral = regexp.MustCompile(`^\d+$`)

// validateSQLID ensures id is a bare bigint literal.
func validateSQLID(id string) error {
	if !bigintLiteral.MatchString(id) {
		return fmt.Errorf("invalid unique_sql_id %q: expected a bigint literal", id)
	}
	return nil
}

// TerminateLimitedSessions builds the anonymous block that terminates up to
// maxCount backends sharing uniqueSQLID. Port of
// TERMINATE_LIMITED_SESSIONS_ANONYMOUS_BLOCK.
func TerminateLimitedSessions(uniqueSQLID string, maxCount int) (string, error) {
	if err := validateSQLID(uniqueSQLID); err != nil {
		return "", err
	}
	return fmt.Sprintf(limitedBlock, uniqueSQLID, maxCount), nil
}

// TerminateLimitedSessionsWithTime additionally requires each backend to be an
// active transaction running longer than timeoutMs. Port of
// TERMINATE_LIMITED_SESSIONS_WITHTIME_ANONYMOUS_BLOCK.
func TerminateLimitedSessionsWithTime(uniqueSQLID string, maxCount, timeoutMs int) (string, error) {
	if err := validateSQLID(uniqueSQLID); err != nil {
		return "", err
	}
	return fmt.Sprintf(limitedWithTimeBlock, uniqueSQLID, maxCount, timeoutMs), nil
}

// TerminateUnlimitedSessions terminates every backend sharing uniqueSQLID. Port
// of TERMINATE_UNLIMITED_SESSIONS_ANONYMOUS_BLOCK.
func TerminateUnlimitedSessions(uniqueSQLID string) (string, error) {
	if err := validateSQLID(uniqueSQLID); err != nil {
		return "", err
	}
	return fmt.Sprintf(unlimitedBlock, uniqueSQLID), nil
}

// TerminateUnlimitedSessionsWithTime terminates every backend sharing
// uniqueSQLID whose active transaction exceeds timeoutMs. Port of
// TERMINATE_UNLIMITED_SESSIONS_WITHTIME_ANONYMOUS_BLOCK.
func TerminateUnlimitedSessionsWithTime(uniqueSQLID string, timeoutMs int) (string, error) {
	if err := validateSQLID(uniqueSQLID); err != nil {
		return "", err
	}
	return fmt.Sprintf(unlimitedWithTimeBlock, uniqueSQLID, timeoutMs), nil
}

// TerminateSessionCmd terminates one backend by pid and session id.
func TerminateSessionCmd(pid, sessionID any) string {
	return fmt.Sprintf("SELECT pg_terminate_session(%v, %v);", pid, sessionID)
}

// TerminateBackendCmd terminates one backend by pid.
func TerminateBackendCmd(pid any) string {
	return fmt.Sprintf("SELECT pg_terminate_backend(%v);", pid)
}

const limitedBlock = `
DO $$
DECLARE
    target_unique_sql_id bigint := %s;
    max_terminate_count integer := %d;
    v_record record;
    v_count integer := 0;
BEGIN
    RAISE NOTICE 'start terminate sessions with unique_sql_id = %%', target_unique_sql_id;

    FOR v_record IN
        SELECT pid
        FROM pg_stat_activity
        WHERE unique_sql_id = target_unique_sql_id
    LOOP
        BEGIN
            PERFORM pg_terminate_backend(v_record.pid);
            RAISE NOTICE 'terminate PID: %%', v_record.pid;
            v_count := v_count + 1;
        EXCEPTION
            WHEN OTHERS THEN
                RAISE NOTICE 'terminate PID: %% failed, error: %%', v_record.pid, SQLERRM;
        END;

        IF v_count >= max_terminate_count THEN
            RAISE NOTICE 'terminate finished, %% sessions in total', v_count;
            EXIT;
        END IF;
    END LOOP;

    RAISE NOTICE 'terminate finished, %% sessions in total', v_count;
END $$;
`

const limitedWithTimeBlock = `
DO $$
DECLARE
    target_unique_sql_id bigint := %s;
    max_terminate_count integer := %d;
    min_execution_time interval := '%d ms'::interval;
    v_record record;
    v_count integer := 0;
BEGIN
    RAISE NOTICE 'start terminate sessions with unique_sql_id = %%', target_unique_sql_id;

    FOR v_record IN
        SELECT pid, xact_start, state
        FROM pg_stat_activity
        WHERE unique_sql_id = target_unique_sql_id
            AND state = 'active'
            AND xact_start IS NOT NULL
            AND (clock_timestamp() - xact_start) > min_execution_time
        ORDER BY xact_start ASC
    LOOP
        BEGIN
            PERFORM pg_terminate_backend(v_record.pid);
            RAISE NOTICE 'terminate PID: %%', v_record.pid;
            v_count := v_count + 1;
        EXCEPTION
            WHEN OTHERS THEN
                RAISE NOTICE 'terminate PID: %% failed, error: %%', v_record.pid, SQLERRM;
        END;

        IF v_count >= max_terminate_count THEN
            RAISE NOTICE 'terminate finished, %% sessions in total', v_count;
            EXIT;
        END IF;
    END LOOP;

    RAISE NOTICE 'terminate finished, %% sessions in total', v_count;
END $$;
`

const unlimitedBlock = `
DO $$
DECLARE
    target_unique_sql_id bigint := %s;
    v_record record;
    v_count integer := 0;
BEGIN
    RAISE NOTICE 'start terminate sessions with unique_sql_id = %%', target_unique_sql_id;

    FOR v_record IN
        SELECT pid
        FROM pg_stat_activity
        WHERE unique_sql_id = target_unique_sql_id
    LOOP
        BEGIN
            PERFORM pg_terminate_backend(v_record.pid);
            RAISE NOTICE 'terminate PID: %%', v_record.pid;
            v_count := v_count + 1;
        EXCEPTION
            WHEN OTHERS THEN
                RAISE NOTICE 'terminate PID: %% failed, error: %%', v_record.pid, SQLERRM;
        END;
    END LOOP;

    RAISE NOTICE 'terminate finished, %% sessions in total', v_count;
END $$;
`

const unlimitedWithTimeBlock = `
DO $$
DECLARE
    target_unique_sql_id bigint := %s;
    min_execution_time interval := '%d ms'::interval;
    v_record record;
    v_count integer := 0;
BEGIN
    RAISE NOTICE 'start terminate sessions with unique_sql_id = %%', target_unique_sql_id;

    FOR v_record IN
        SELECT pid, xact_start, state
        FROM pg_stat_activity
        WHERE unique_sql_id = target_unique_sql_id
            AND state = 'active'
            AND xact_start IS NOT NULL
            AND (clock_timestamp() - xact_start) > min_execution_time
        ORDER BY xact_start ASC
    LOOP
        BEGIN
            PERFORM pg_terminate_backend(v_record.pid);
            RAISE NOTICE 'terminate PID: %%', v_record.pid;
            v_count := v_count + 1;
        EXCEPTION
            WHEN OTHERS THEN
                RAISE NOTICE 'terminate PID: %% failed, error: %%', v_record.pid, SQLERRM;
        END;
    END LOOP;

    RAISE NOTICE 'terminate finished, %% sessions in total', v_count;
END $$;
`
