-- Apply once with the mysql client (supports DELIMITER), after business tables.
-- DDL implicitly commits in MySQL: schedule migration before enabling workers.
CREATE TABLE buyer_search_jobs (
 lot_id varchar(64) NOT NULL PRIMARY KEY,
 room_id varchar(64) NOT NULL,
 main_account_id varchar(64) NOT NULL,
 semantic_revision bigint unsigned NOT NULL DEFAULT 1,
 metadata_revision bigint unsigned NOT NULL DEFAULT 1,
 deleted boolean NOT NULL DEFAULT FALSE,
 state varchar(16) NOT NULL DEFAULT 'pending',
 lease_token varchar(64) NOT NULL DEFAULT '',
 lease_until datetime(6) NULL,
 attempts int NOT NULL DEFAULT 0,
 next_attempt_at datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 error_class varchar(32) NOT NULL DEFAULT '',
 changed_at datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 INDEX buyer_search_claim(state,next_attempt_at,lot_id),
 INDEX buyer_search_lease(state,lease_until),
 INDEX buyer_search_room(room_id,lot_id),
 INDEX buyer_search_owner(main_account_id,lot_id)
) ENGINE=InnoDB;
CREATE TABLE buyer_search_scope_jobs (
 scope_kind varchar(16) NOT NULL,
 scope_id varchar(64) NOT NULL,
 last_lot_id varchar(64) NOT NULL DEFAULT '',
 state varchar(16) NOT NULL DEFAULT 'pending',
 changed_at datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
 PRIMARY KEY(scope_kind,scope_id),
 INDEX buyer_search_scope_claim(state,changed_at)
) ENGINE=InnoDB;
DELIMITER $$
-- Persistent checkpoint for named generation backfills/reconciliation runs.
-- Created outside DELIMITER blocks using the same terminator.
CREATE TABLE buyer_search_backfill_progress (
 name varchar(64) PRIMARY KEY,last_lot_id varchar(64) NOT NULL DEFAULT '',
 state varchar(16) NOT NULL DEFAULT 'pending',updated_at datetime(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB$$
CREATE PROCEDURE buyer_search_enqueue(IN lid varchar(64),IN rid varchar(64),IN mid varchar(64),IN sem boolean,IN gone boolean)
BEGIN
 INSERT INTO buyer_search_jobs(lot_id,room_id,main_account_id,deleted)
 VALUES(lid,rid,mid,gone)
 ON DUPLICATE KEY UPDATE room_id=rid,main_account_id=mid,
 semantic_revision=semantic_revision+IF(sem,1,0),metadata_revision=metadata_revision+1,
 deleted=gone,state='pending',lease_token='',lease_until=NULL,attempts=0,
 next_attempt_at=CURRENT_TIMESTAMP(6),error_class='',changed_at=CURRENT_TIMESTAMP(6);
END$$
CREATE PROCEDURE buyer_search_enqueue_scope(IN kind varchar(16),IN sid varchar(64))
BEGIN
 INSERT INTO buyer_search_scope_jobs(scope_kind,scope_id) VALUES(kind,sid)
 ON DUPLICATE KEY UPDATE last_lot_id='',state='pending',changed_at=CURRENT_TIMESTAMP(6);
END$$
CREATE TRIGGER buyer_search_lot_insert AFTER INSERT ON auction_lots FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue(NEW.id,NEW.room_id,NEW.main_account_id,TRUE,FALSE);
END$$
CREATE TRIGGER buyer_search_lot_update AFTER UPDATE ON auction_lots FOR EACH ROW
BEGIN
 DECLARE sem boolean;
 SET sem=NOT(OLD.title<=>NEW.title) OR NOT(OLD.description<=>NEW.description)
  OR NOT(JSON_EXTRACT(OLD.payload,'$.category')<=>JSON_EXTRACT(NEW.payload,'$.category'))
  OR NOT(JSON_EXTRACT(OLD.payload,'$.tags')<=>JSON_EXTRACT(NEW.payload,'$.tags'));
 IF sem OR NOT(OLD.status<=>NEW.status) OR NOT(OLD.room_id<=>NEW.room_id)
  OR NOT(OLD.main_account_id<=>NEW.main_account_id) THEN
  CALL buyer_search_enqueue(NEW.id,NEW.room_id,NEW.main_account_id,sem,FALSE);
 END IF;
END$$
CREATE TRIGGER buyer_search_lot_delete AFTER DELETE ON auction_lots FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue(OLD.id,OLD.room_id,OLD.main_account_id,TRUE,TRUE);
END$$
CREATE TRIGGER buyer_search_room_update AFTER UPDATE ON auction_rooms FOR EACH ROW
BEGIN
 IF NOT(OLD.status<=>NEW.status) OR NOT(OLD.main_account_id<=>NEW.main_account_id) THEN
  CALL buyer_search_enqueue_scope('room',NEW.id);
 END IF;
END$$
CREATE TRIGGER buyer_search_room_delete AFTER DELETE ON auction_rooms FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue_scope('room',OLD.id);
END$$
CREATE TRIGGER buyer_search_owner_update AFTER UPDATE ON auction_users FOR EACH ROW
BEGIN
 IF NOT(OLD.status<=>NEW.status) THEN CALL buyer_search_enqueue_scope('owner',NEW.id); END IF;
END$$
CREATE TRIGGER buyer_search_owner_delete AFTER DELETE ON auction_users FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue_scope('owner',OLD.id);
END$$
CREATE TRIGGER buyer_search_role_insert AFTER INSERT ON auction_user_roles FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue_scope('owner',NEW.user_id);
END$$
CREATE TRIGGER buyer_search_role_update AFTER UPDATE ON auction_user_roles FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue_scope('owner',OLD.user_id);
 IF NOT(OLD.user_id<=>NEW.user_id) THEN CALL buyer_search_enqueue_scope('owner',NEW.user_id); END IF;
END$$
CREATE TRIGGER buyer_search_role_delete AFTER DELETE ON auction_user_roles FOR EACH ROW
BEGIN
 CALL buyer_search_enqueue_scope('owner',OLD.user_id);
END$$
DELIMITER ;
-- Initial backfill is keyset-paged by the admin command; do not run an
-- unbounded INSERT SELECT in the auction writer transaction.
