CREATE DATABASE diagnostic_game;
CREATE DATABASE diagnostic_hidden;
CREATE TABLE diagnostic_game.players (id INT PRIMARY KEY, name VARCHAR(64));
INSERT INTO diagnostic_game.players VALUES (1, 'fixture');

CREATE USER 'diagnostic_backup'@'%' IDENTIFIED BY 'diagnostics_backup';
GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON diagnostic_game.* TO 'diagnostic_backup'@'%';
-- A grant on an absent schema allows USE to report "unknown database" rather
-- than masking existence with access denied, as it does for hidden schemas.
GRANT SELECT ON missing_fixture_database.* TO 'diagnostic_backup'@'%';

CREATE USER 'diagnostic_readonly'@'%' IDENTIFIED BY 'diagnostics_readonly';
GRANT SELECT ON diagnostic_game.* TO 'diagnostic_readonly'@'%';
