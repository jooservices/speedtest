-- UI seed for the real-mode pages: five independent nodes, a week of half-hourly results, two IP changes, a few alert events.
INSERT INTO nodes (id,name,label,provider,plan,expected_down_mbps,expected_up_mbps,color_slot,host,agent,last_seen_at) VALUES
 ('10000000-0000-4000-8000-000000000001','VM1','Viettel Fast 50','Viettel','Fast 50',500,100,1,'vm1','speedtest-agent/1.0.0',now()),
 ('10000000-0000-4000-8000-000000000002','VM2','Viettel Pro 600','Viettel','Pro 600',600,150,2,'vm2','speedtest-agent/1.0.0',now()),
 ('10000000-0000-4000-8000-000000000003','VM3','VNPT Home 5 Super','VNPT','Home 5',300,100,3,'vm3','speedtest-agent/1.0.0',now()),
 ('10000000-0000-4000-8000-000000000004','VM4','VNPT Home 2','VNPT','Home 2',150,50,4,'vm4','speedtest-agent/1.0.0',now() - interval '1 hour'),
 ('10000000-0000-4000-8000-000000000005','VM5','All connections (load balanced)','Router','LB',1500,300,5,'vm5','speedtest-agent/1.0.0',now());
INSERT INTO schedules (node_id, interval_minutes, offset_minutes) SELECT id, 30, (color_slot-1)*6 FROM nodes;
INSERT INTO engine_schedules SELECT node_id,'ookla',interval_minutes,offset_minutes,enabled,revision FROM schedules;
INSERT INTO ip_addresses (ip,geo,source,looked_up_at) VALUES
 ('203.0.113.11','{"city":"Hanoi","region":"Hanoi","country":"VN","latitude":21.0285,"longitude":105.8542,"time_zone":"Asia/Ho_Chi_Minh","accuracy_radius":20,"asn":7552,"organisation":"Viettel Group"}','GeoLite2',now()),
 ('203.0.113.12','{"city":"Ho Chi Minh City","region":"Ho Chi Minh","country":"VN","latitude":10.8231,"longitude":106.6297,"time_zone":"Asia/Ho_Chi_Minh","accuracy_radius":20,"asn":7552,"organisation":"Viettel Group"}','GeoLite2',now()),
 ('198.51.100.21','{"city":"Da Nang","region":"Da Nang","country":"VN","latitude":16.0544,"longitude":108.2022,"time_zone":"Asia/Ho_Chi_Minh","accuracy_radius":50,"asn":45899,"organisation":"VNPT"}','GeoLite2',now()),
 ('198.51.100.22','{"city":"Hue","region":"Thua Thien Hue","country":"VN","latitude":16.4637,"longitude":107.5909,"time_zone":"Asia/Ho_Chi_Minh","accuracy_radius":50,"asn":45899,"organisation":"VNPT"}','GeoLite2',now());
INSERT INTO results (id,node_id,ts,status,down_mbps,up_mbps,ping_ms,jitter_ms,loss_percent,server_id,server,ip,threads,bytes_down,bytes_up,duration_seconds,error)
SELECT gen_random_uuid(), n.id, now() - (g * interval '30 minutes') - (n.color_slot * interval '1 minute'),
  CASE WHEN g % 97 = 0 THEN 'failed' WHEN g % 41 = 0 THEN 'degraded' ELSE 'ok' END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE n.expected_down_mbps * (0.62 + 0.33 * abs(sin(g / 7.0 + n.color_slot))) END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE n.expected_up_mbps * (0.6 + 0.35 * abs(cos(g / 9.0 + n.color_slot))) END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE 9 + 14 * abs(sin(g / 5.0 + n.color_slot)) END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE 1 + 3 * abs(cos(g / 6.0)) END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE (g % 5) * 0.05 END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE 9 END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE '{"id":9,"name":"Viettel","city":"Hanoi","sponsor":"Viettel Group"}'::jsonb END,
  CASE WHEN n.color_slot <= 2 THEN (CASE WHEN g > 150 THEN '203.0.113.11' ELSE '203.0.113.12' END)::inet ELSE (CASE WHEN g > 200 THEN '198.51.100.21' ELSE '198.51.100.22' END)::inet END,
  4, CASE WHEN g % 97 = 0 THEN NULL ELSE 400000000 + g END,
  CASE WHEN g % 97 = 0 THEN NULL ELSE 50000000 + g END, 28.0,
  CASE WHEN g % 97 = 0 THEN 'The test server did not answer in time' END
FROM nodes n CROSS JOIN generate_series(0, 335) g;
INSERT INTO node_ip_addresses (node_id, ip, tests, first_seen, last_seen, speed_tests, down_sum, up_sum)
SELECT node_id, ip, count(*), min(ts), max(ts), count(*) FILTER (WHERE status <> 'failed'), coalesce(sum(down_mbps::numeric),0), coalesce(sum(up_mbps::numeric),0) FROM results GROUP BY node_id, ip;
INSERT INTO node_ip_history (id,node_id,ts,from_ip,to_ip)
SELECT gen_random_uuid(), node_id, first_seen, NULL, ip FROM node_ip_addresses a WHERE first_seen = (SELECT min(first_seen) FROM node_ip_addresses b WHERE b.node_id = a.node_id);
INSERT INTO node_ip_history (id,node_id,ts,from_ip,to_ip)
SELECT gen_random_uuid(), a.node_id, a.first_seen, (SELECT ip FROM node_ip_addresses b WHERE b.node_id = a.node_id AND b.first_seen < a.first_seen ORDER BY first_seen DESC LIMIT 1), a.ip
FROM node_ip_addresses a WHERE a.first_seen > (SELECT min(first_seen) FROM node_ip_addresses b WHERE b.node_id = a.node_id);
INSERT INTO alert_rules (id,name,metric,op,value,for_tests,severity,channels,enabled) VALUES
 ('20000000-0000-4000-8000-000000000001','Download below 50% of expected','down_pct','<',50,3,'warn',ARRAY['email'],true),
 ('20000000-0000-4000-8000-000000000002','Test failed twice in a row','failed','=',1,2,'crit',ARRAY['email','telegram'],true);
INSERT INTO alert_events (id,rule_id,node_id,severity,ts,title,detail,acked) VALUES
 (gen_random_uuid(),'20000000-0000-4000-8000-000000000001','10000000-0000-4000-8000-000000000003','warn',now() - interval '5 hours','VM3 download is slow','Download stayed below 50% of the expected speed for 3 tests',false),
 (gen_random_uuid(),'20000000-0000-4000-8000-000000000002','10000000-0000-4000-8000-000000000004','crit',now() - interval '2 hours','VM4 tests are failing','2 tests in a row failed',false);
