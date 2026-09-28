CREATE USER IF NOT EXISTS 'fivem'@'%' IDENTIFIED BY 'fivem_dev_password';
GRANT ALL PRIVILEGES ON `es_extended`.* TO 'fivem'@'%';

FLUSH PRIVILEGES;
