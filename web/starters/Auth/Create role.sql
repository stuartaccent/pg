-- Swap in the names and password first. What's run is kept in History, so
-- change the password afterwards if that matters here.
create role app_user login password 'change-me';
grant connect on database mydb to app_user;
grant usage on schema public to app_user;
grant select on all tables in schema public to app_user;
alter default privileges in schema public grant select on tables to app_user;
