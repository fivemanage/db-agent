USE `es_extended`;

INSERT INTO `users`
  (`identifier`, `firstname`, `lastname`, `name`, `dateofbirth`, `sex`, `height`, `phone_number`, `group`, `money`, `job`, `job_grade`, `loadout`, `position`, `inventory`, `skin`, `is_dead`, `last_login`)
VALUES
  ('license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', 'Marcus', 'Bell', 'Marcus Bell', '1988-03-14', 'm', 186, '5550104', 'admin', 482300, 'police', 4, '[{"label":"Pistol","name":"WEAPON_PISTOL","components":[],"tint":0}]', '{"x":-445.7,"y":6016.5,"z":31.7,"heading":275.0}', '{"bread":2,"water":1}', NULL, 0, DATE_SUB(NOW(), INTERVAL 25 MINUTE)),
  ('steam:110000103f5e6d7c', 'Sofia', 'Reyes', 'Sofia Reyes', '1995-07-22', 'f', 165, '5550119', 'user', 215800, 'ambulance', 2, '[]', '{"x":-269.4,"y":-955.3,"z":31.2,"heading":183.5}', '{"bandage":5,"water":3}', '{"sex":1,"face":2,"hair":24}', 0, DATE_SUB(NOW(), INTERVAL 2 HOUR)),
  ('license:8b2c4d6e8f0a1b3c5d7e9f1a3b5c7d9e0f1a2b3c', 'Dmitri', 'Volkov', 'Dmitri Volkov', '1979-11-02', 'm', 178, '5550131', 'user', 96500, 'mechanic', 3, '[]', '{"x":729.6,"y":-1088.3,"z":22.2,"heading":90.0}', '{"fixkit":4,"radio":1}', NULL, 0, DATE_SUB(NOW(), INTERVAL 6 HOUR)),
  ('license:1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d', 'Tina', 'Chen', 'Tina Chen', '2001-05-30', 'f', 158, '5550146', 'user', 12400, 'unemployed', 0, '[]', '{"x":-1034.6,"y":-2732.5,"z":13.8,"heading":45.0}', NULL, NULL, 0, DATE_SUB(NOW(), INTERVAL 1 DAY)),
  ('license:c0ffee1234567890abcdef1234567890abcdef12', 'Bob', 'Maples', 'Bob Maples', '1975-09-09', 'm', 174, '5550152', 'user', 2100, 'unemployed', 0, '[]', NULL, NULL, NULL, 1, NULL),
  ('steam:110000111aa2bb3cc', 'Jamal', 'Carter', 'Jamal Carter', '1993-01-18', 'm', 181, '5550167', 'user', 78200, 'police', 1, '[]', '{"x":-448.2,"y":6012.9,"z":31.7,"heading":180.0}', '{"donut":6,"radio":1}', NULL, 0, DATE_SUB(NOW(), INTERVAL 4 HOUR)),
  ('license:9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c', 'Elena', 'Rossi', 'Elena Rossi', '1990-12-25', 'f', 170, '5550178', 'user', 340000, 'cardealer', 1, '[]', '{"x":-56.8,"y":-1096.6,"z":26.4,"heading":250.0}', NULL, '{"sex":1,"face":0,"hair":45}', 0, DATE_SUB(NOW(), INTERVAL 8 HOUR)),
  ('license:4e5d6c7b8a901a2b3c4d5e6f70819a2b3c4d5e6f', 'Willie', 'Norton', 'Willie Norton', '1983-06-06', 'm', 169, '5550183', 'user', 740, 'unemployed', 0, '[]', '{"x":195.2,"y":-933.4,"z":30.7,"heading":144.0}', '{"lockpick":2}', NULL, 0, DATE_SUB(NOW(), INTERVAL 12 DAY)),
  ('steam:110000105b4c3d2e', 'Amy', 'Woods', 'Amy Woods', '1986-08-11', 'f', 163, '5550195', 'mod', 512000, 'ambulance', 4, '[]', '{"x":-269.4,"y":-955.3,"z":31.2,"heading":183.5}', '{"medkit":8,"radio":1}', NULL, 0, DATE_SUB(NOW(), INTERVAL 90 MINUTE)),
  ('license:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c', 'Hugo', 'Lima', 'Hugo Lima', '1999-02-28', 'm', 177, '5550201', 'user', 33700, 'mechanic', 1, '[]', '{"x":1172.9,"y":2640.5,"z":37.9,"heading":10.0}', '{"fixkit":1}', NULL, 0, DATE_SUB(NOW(), INTERVAL 3 DAY));

INSERT INTO `jobs` (`name`, `label`) VALUES
  ('unemployed', 'Unemployed'),
  ('police', 'Police'),
  ('ambulance', 'Ambulance'),
  ('mechanic', 'Mechanic'),
  ('cardealer', 'Car Dealer');

INSERT INTO `job_grades` (`job_name`, `grade`, `name`, `label`, `salary`) VALUES
  ('unemployed', 0, 'unemployed', 'Unemployed', 0),
  ('police', 0, 'recruit', 'Recruit', 500),
  ('police', 1, 'officer', 'Officer', 750),
  ('police', 2, 'sergeant', 'Sergeant', 1000),
  ('police', 3, 'lieutenant', 'Lieutenant', 1250),
  ('police', 4, 'boss', 'Chief', 1500),
  ('ambulance', 0, 'trainee', 'Paramedic Trainee', 500),
  ('ambulance', 1, 'paramedic', 'Paramedic', 750),
  ('ambulance', 2, 'doctor', 'Doctor', 1000),
  ('ambulance', 3, 'surgeon', 'Surgeon', 1250),
  ('ambulance', 4, 'boss', 'Chief Surgeon', 1500),
  ('mechanic', 0, 'apprentice', 'Apprentice', 300),
  ('mechanic', 1, 'mechanic', 'Mechanic', 500),
  ('mechanic', 2, 'senior', 'Senior Mechanic', 700),
  ('mechanic', 3, 'boss', 'Shop Owner', 1000),
  ('cardealer', 0, 'salesman', 'Salesman', 400),
  ('cardealer', 1, 'boss', 'Dealer Principal', 800);

INSERT INTO `owned_vehicles` (`owner`, `plate`, `vehicle`, `type`, `job`, `stored`) VALUES
  ('license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', 'BCU 45T2', '{"model":"sultanrs","plate":"BCU 45T2","fuelLevel":93.0,"engineHealth":942.5,"bodyHealth":880.0,"color1":[27,27,27],"color2":[27,27,27]}', 'car', NULL, 1),
  ('license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', 'PD 8842', '{"model":"police","plate":"PD 8842","fuelLevel":78.0,"engineHealth":1000.0,"bodyHealth":1000.0}', 'car', 'police', 0),
  ('steam:110000103f5e6d7c', 'AMB 071', '{"model":"ambulance","plate":"AMB 071","fuelLevel":64.5,"engineHealth":890.0,"bodyHealth":950.0}', 'car', 'ambulance', 0),
  ('license:8b2c4d6e8f0a1b3c5d7e9f1a3b5c7d9e0f1a2b3c', 'TOW 11X', '{"model":"towtruck","plate":"TOW 11X","fuelLevel":52.0,"engineHealth":765.0,"bodyHealth":610.0}', 'car', 'mechanic', 0),
  ('license:8b2c4d6e8f0a1b3c5d7e9f1a3b5c7d9e0f1a2b3c', 'LS 9090', '{"model":"bison","plate":"LS 9090","fuelLevel":88.0,"engineHealth":980.0,"bodyHealth":940.0}', 'car', NULL, 1),
  ('license:1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d', 'BMX 4LFE', '{"model":"bmx","plate":"BMX 4LFE"}', 'bicycle', NULL, 1),
  ('steam:110000111aa2bb3cc', 'PD 2291', '{"model":"police2","plate":"PD 2291","fuelLevel":85.0,"engineHealth":995.0,"bodyHealth":980.0}', 'car', 'police', 0),
  ('license:9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c', 'VIP 001', '{"model":"comet6","plate":"VIP 001","fuelLevel":99.0,"engineHealth":1000.0,"bodyHealth":1000.0,"color1":[212,0,0],"color2":[212,0,0]}', 'car', NULL, 0),
  ('steam:110000105b4c3d2e', 'AMB 011', '{"model":"ambulance","plate":"AMB 011","fuelLevel":71.0,"engineHealth":930.0,"bodyHealth":975.0}', 'car', 'ambulance', 0),
  ('license:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c', 'LS 4412', '{"model":"sabregt","plate":"LS 4412","fuelLevel":41.0,"engineHealth":703.5,"bodyHealth":520.0}', 'car', NULL, 1);

INSERT INTO `owned_properties` (`owner`, `name`, `price`, `rented`) VALUES
  ('steam:110000103f5e6d7c', 'apartment1', 25000, 0),
  ('license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', 'mansion', 750000, 0),
  ('license:1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d', 'motel', 500, 1);

INSERT INTO `addon_accounts` (`owner`, `name`, `money`) VALUES
  (NULL, 'society_police', 250000),
  (NULL, 'society_ambulance', 180000),
  (NULL, 'society_mechanic', 95000),
  (NULL, 'society_cardealer', 42000),
  ('license:8b2c4d6e8f0a1b3c5d7e9f1a3b5c7d9e0f1a2b3c', 'black_money', 12500),
  ('license:4e5d6c7b8a901a2b3c4d5e6f70819a2b3c4d5e6f', 'black_money', 3400);

INSERT INTO `addon_inventory` (`owner`, `name`, `count`) VALUES
  ('license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', 'donut', 6),
  ('license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', 'radio', 1),
  ('steam:110000103f5e6d7c', 'medkit', 2),
  ('steam:110000103f5e6d7c', 'water', 4),
  ('license:8b2c4d6e8f0a1b3c5d7e9f1a3b5c7d9e0f1a2b3c', 'fixkit', 4),
  ('license:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c', 'fixkit', 1),
  ('license:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c', 'radio', 1),
  ('license:1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d', 'bread', 2),
  ('license:1a2b3c4d5e6f70819a2b3c4d5e6f70819a2b3c4d', 'water', 2);

INSERT INTO `datastore` (`name`, `owner`, `data`) VALUES
  ('user_licenses', 'license:5f3a9c1d2e4b60718293a4b5c6d7e8f9a0b1c2d3', '{"driver":true,"weapon":true,"pilot":false}'),
  ('user_licenses', 'steam:110000103f5e6d7c', '{"driver":true,"weapon":false}'),
  ('trunk', 'BCU 45T2', '{"items":[{"name":"sparetire","count":1}],"money":0,"weapons":[]}'),
  ('glovebox', 'BCU 45T2', '{"items":[{"name":"radio","count":1}],"money":0,"weapons":[]}'),
  ('property', 'steam:110000103f5e6d7c', '{"items":[],"weapons":[],"money":0}');

INSERT INTO `bans` (`identifier`, `reason`, `banner`, `expires_at`, `created_at`) VALUES
  ('license:4e5d6c7b8a901a2b3c4d5e6f70819a2b3c4d5e6f', 'Aimbot / ESP — evidence clip submitted', 'txAdmin', NULL, DATE_SUB(NOW(), INTERVAL 5 DAY)),
  ('license:deadbeef42deadbeef42deadbeef42deadbeef4', 'RDM + VDM (5th offense)', 'Marcus Bell', DATE_ADD(NOW(), INTERVAL 30 DAY), DATE_SUB(NOW(), INTERVAL 2 DAY));
