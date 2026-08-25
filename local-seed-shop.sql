SET NAMES utf8mb4;
SET @now_ms = UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3)) * 1000;

INSERT INTO shop_products (id,title,subtitle,description,category,shop_name,main_image_url,detail_image_urls,tags,badges,price_amount,original_price_amount,currency,sold_label,live,status,created_at_unix_ms,updated_at_unix_ms)
VALUES
('imperial-green-jade-bangle','冰阳绿翡翠手镯 正圈饱满细腻起光','直播间同款，支持上手看货','精选冰阳绿翡翠手镯，圈口细腻，适合送礼和自戴。','珠宝玉石','Ggboy珠宝严选','/shop-assets/auction-lots/imperial-green-jade-bangle.png',JSON_ARRAY('/shop-assets/auction-lots/imperial-green-jade-bangle.png'),JSON_ARRAY('翡翠手镯','正在直播','包邮'),JSON_ARRAY('直播中','旗舰'),47000,59900,'CNY','3.8万+',1,'active',@now_ms,@now_ms),
('white-hetian-jade-bangle','白月光和田玉手镯 温润通透日常款','低价开拍，同城优先发货','温润白玉手镯，颜色干净，适合日常搭配。','珠宝玉石','Yexieer珠宝店','/shop-assets/auction-lots/white-hetian-jade-bangle.png',JSON_ARRAY('/shop-assets/auction-lots/white-hetian-jade-bangle.png'),JSON_ARRAY('和田玉','低价开拍'),JSON_ARRAY('补贴'),8800,12900,'CNY','6.4万+',0,'active',@now_ms,@now_ms),
('carved-jade-pendant','天然翡翠平安扣项链 冰润飘花吊坠','送礼好物，附精美礼盒','冰润翡翠平安扣，搭配简洁项链，适合作为礼物。','项链吊坠','Ggboy珠宝严选','/shop-assets/auction-lots/carved-jade-pendant-necklace.png',JSON_ARRAY('/shop-assets/auction-lots/carved-jade-pendant-necklace.png'),JSON_ARRAY('项链','适合送礼'),JSON_ARRAY('热卖'),19900,25900,'CNY','1.9万+',1,'active',@now_ms,@now_ms),
('freshwater-pearl-necklace','淡水珍珠项链 近圆强光通勤气质款','新娘礼物与日常通勤都合适','淡水珍珠项链，强光近圆，经典百搭。','项链吊坠','珍珠小姐旗舰店','/shop-assets/auction-lots/freshwater-pearl-necklace.png',JSON_ARRAY('/shop-assets/auction-lots/freshwater-pearl-necklace.png'),JSON_ARRAY('珍珠','送礼'),JSON_ARRAY('新品'),36800,42900,'CNY','2.6万+',0,'active',@now_ms,@now_ms),
('ruby-diamond-bracelet','红宝石钻石手链 18K金精致叠戴款','小众设计，节日礼物','红宝石与钻石点缀，适合叠戴和节日送礼。','手链手串','璀璨宝石馆','/shop-assets/auction-lots/ruby-diamond-gold-bracelet.png',JSON_ARRAY('/shop-assets/auction-lots/ruby-diamond-gold-bracelet.png'),JSON_ARRAY('红宝石','18K金'),JSON_ARRAY('限时'),69900,89900,'CNY','9800+',0,'active',@now_ms,@now_ms),
('sapphire-diamond-necklace','蓝宝石钻石项链 锁骨链高级感礼盒装','直播精选，支持保价','蓝宝石钻石锁骨链，细节精致，礼盒装发货。','项链吊坠','璀璨宝石馆','/shop-assets/auction-lots/sapphire-diamond-necklace.png',JSON_ARRAY('/shop-assets/auction-lots/sapphire-diamond-necklace.png'),JSON_ARRAY('蓝宝石','礼盒'),JSON_ARRAY('直播中'),75900,99900,'CNY','1.2万+',1,'active',@now_ms,@now_ms)
ON DUPLICATE KEY UPDATE title=VALUES(title), subtitle=VALUES(subtitle), description=VALUES(description), category=VALUES(category), shop_name=VALUES(shop_name), main_image_url=VALUES(main_image_url), detail_image_urls=VALUES(detail_image_urls), tags=VALUES(tags), badges=VALUES(badges), price_amount=VALUES(price_amount), original_price_amount=VALUES(original_price_amount), currency=VALUES(currency), sold_label=VALUES(sold_label), live=VALUES(live), status=VALUES(status), updated_at_unix_ms=@now_ms;

INSERT INTO shop_skus (id,product_id,name,price_amount,currency,stock)
VALUES
('sku-jade-bangle-54','imperial-green-jade-bangle','54圈口',47000,'CNY',88),
('sku-jade-bangle-56','imperial-green-jade-bangle','56圈口',49900,'CNY',65),
('sku-white-jade-55','white-hetian-jade-bangle','55圈口',8800,'CNY',120),
('sku-white-jade-57','white-hetian-jade-bangle','57圈口',9300,'CNY',80),
('sku-jade-pendant-gold','carved-jade-pendant','金色链条',19900,'CNY',76),
('sku-jade-pendant-silver','carved-jade-pendant','银色链条',18900,'CNY',72),
('sku-pearl-42','freshwater-pearl-necklace','42cm',36800,'CNY',45),
('sku-pearl-45','freshwater-pearl-necklace','45cm',38900,'CNY',38),
('sku-ruby-bracelet-s','ruby-diamond-bracelet','15cm',69900,'CNY',18),
('sku-ruby-bracelet-m','ruby-diamond-bracelet','16.5cm',72900,'CNY',16),
('sku-sapphire-necklace','sapphire-diamond-necklace','礼盒装',75900,'CNY',22)
ON DUPLICATE KEY UPDATE product_id=VALUES(product_id), name=VALUES(name), price_amount=VALUES(price_amount), currency=VALUES(currency), stock=VALUES(stock);
