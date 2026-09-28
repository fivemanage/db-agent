CREATE DATABASE IF NOT EXISTS `es_extended` DEFAULT CHARACTER SET utf8mb4;
USE `es_extended`;

CREATE TABLE `users` (
  `identifier` VARCHAR(60) NOT NULL,
  `firstname` VARCHAR(40) DEFAULT NULL,
  `lastname` VARCHAR(40) DEFAULT NULL,
  `name` VARCHAR(80) DEFAULT NULL,
  `dateofbirth` VARCHAR(15) DEFAULT NULL,
  `sex` VARCHAR(1) DEFAULT NULL,
  `height` INT DEFAULT NULL,
  `phone_number` VARCHAR(12) DEFAULT NULL,
  `group` VARCHAR(50) NOT NULL DEFAULT 'user',
  `money` BIGINT NOT NULL DEFAULT 0,
  `job` VARCHAR(60) NOT NULL DEFAULT 'unemployed',
  `job_grade` INT NOT NULL DEFAULT 0,
  `loadout` LONGTEXT DEFAULT NULL,
  `position` LONGTEXT DEFAULT NULL,
  `inventory` LONGTEXT DEFAULT NULL,
  `skin` LONGTEXT DEFAULT NULL,
  `is_dead` TINYINT(1) NOT NULL DEFAULT 0,
  `last_login` TIMESTAMP NULL DEFAULT NULL,
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `license` VARCHAR(80) DEFAULT NULL,
  PRIMARY KEY (`identifier`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `jobs` (
  `name` VARCHAR(50) NOT NULL,
  `label` VARCHAR(60) NOT NULL,
  PRIMARY KEY (`name`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `job_grades` (
  `job_name` VARCHAR(50) NOT NULL,
  `grade` INT NOT NULL,
  `name` VARCHAR(60) NOT NULL,
  `label` VARCHAR(60) NOT NULL,
  `salary` INT NOT NULL DEFAULT 0,
  PRIMARY KEY (`job_name`, `grade`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `owned_vehicles` (
  `owner` VARCHAR(60) NOT NULL,
  `plate` VARCHAR(12) NOT NULL,
  `vehicle` LONGTEXT DEFAULT NULL,
  `type` ENUM('car', 'boat', 'aircraft', 'bicycle') NOT NULL DEFAULT 'car',
  `job` VARCHAR(60) DEFAULT NULL,
  `stored` TINYINT(1) NOT NULL DEFAULT 0,
  PRIMARY KEY (`owner`, `plate`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `owned_properties` (
  `id` INT NOT NULL AUTO_INCREMENT,
  `owner` VARCHAR(60) NOT NULL,
  `name` VARCHAR(255) NOT NULL,
  `price` DOUBLE NOT NULL,
  `rented` INT NOT NULL DEFAULT 0,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `addon_accounts` (
  `owner` VARCHAR(60) DEFAULT NULL,
  `name` VARCHAR(60) NOT NULL,
  `money` INT NOT NULL DEFAULT 0,
  KEY `owner` (`owner`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `addon_inventory` (
  `owner` VARCHAR(60) DEFAULT NULL,
  `name` VARCHAR(60) NOT NULL,
  `count` INT NOT NULL DEFAULT 0,
  KEY `owner` (`owner`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `datastore` (
  `name` VARCHAR(60) NOT NULL,
  `owner` VARCHAR(60) NOT NULL,
  `data` LONGTEXT DEFAULT NULL,
  PRIMARY KEY (`name`, `owner`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

CREATE TABLE `bans` (
  `id` INT NOT NULL AUTO_INCREMENT,
  `identifier` VARCHAR(60) NOT NULL,
  `reason` TEXT DEFAULT NULL,
  `banner` VARCHAR(80) DEFAULT NULL,
  `expires_at` TIMESTAMP NULL DEFAULT NULL,
  `created_at` TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
