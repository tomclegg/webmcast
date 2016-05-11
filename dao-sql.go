package main

import (
	"database/sql"
	"reflect"
	"sync"
)

type sqlDAO struct {
	sql.DB
	// The string written to `streams.server` of streams owned by this server.
	localhost string
	// Security tokens of active streams owned by this server.
	// Some broadcasting software (*cough* gstreamer *cough*) wraps each frame
	// in a separate request, which may or may not overload the database...
	streamTokenLock sync.RWMutex
	streamTokens    map[string]string

	prepared struct {
		UserExists      *sql.Stmt "select 1 from users where login = ? or email = ?"
		NewUser         *sql.Stmt "insert into users(actoken, sectoken, name, login, email, pwhash) values(?, ?, ?, ?, ?, ?)"
		NewStream       *sql.Stmt "insert into streams(user) values(?)"
		ActivateUser    *sql.Stmt "update users set actoken = NULL where id = ? and actoken = ?"
		GetUserID       *sql.Stmt "select id, pwhash from users where login = ?"
		GetUserInfo     *sql.Stmt "select name, login, email, pwhash, about, actoken, sectoken from users where id = ?"
		GetStreamInfo   *sql.Stmt "select users.name, about, email, streams.name, server, video, audio, width, height, streams.id from users join streams on users.id = streams.user where login = ?"
		SetStreamToken  *sql.Stmt "update users set sectoken = ? where id = ?"
		SetStreamName   *sql.Stmt "update streams set name = ? where user = ?"
		SetStreamTracks *sql.Stmt "update streams set video = ?, audio = ?, width = ?, height = ? where user in (select id from users where login = ?)"
		GetStreamPanels *sql.Stmt "select text, image from panels where stream = ?"
		AddStreamPanel  *sql.Stmt "insert into panels(stream, text) select id, ? from streams where user = ?"
		SetStreamPanel  *sql.Stmt "update panels set text = ? where id in (select id from panels where stream in (select id from streams where user = ?) limit 1 offset ?)"
		DelStreamPanel  *sql.Stmt "delete from panels where id in (select id from panels where stream in (select id from streams where user = ?) limit 1 offset ?)"
		GetStreamAuth   *sql.Stmt "select server, sectoken, actoken is null from users join streams on users.id = streams.user where users.login = ?"
		GetStreamServer *sql.Stmt "select server from streams where user in (select id from users where login = ?)"
		SetStreamServer *sql.Stmt "update streams set server = ? where server is null and user in (select id from users where login = ? and actoken is null and sectoken = ?)"
		DelStreamServer *sql.Stmt "update streams set server = null where user in (select id from users where login = ?)"
	}
}

const sqlSchema = `
create table if not exists users (
    id           integer      not null primary key,
    actoken      varchar(64),
    sectoken     varchar(64)  not null,
    name         varchar(256) not null,
    login        varchar(256) not null,
    email        varchar(256) not null,
    pwhash       varchar(256) not null,
    about        text         not null default "",
    unique(login), unique(email)
);

create table if not exists streams (
    id         integer      not null primary key,
    user       integer      not null,
    video      boolean      not null default 1,
    audio      boolean      not null default 1,
    width      integer      not null default 0,
    height     integer      not null default 0,
    name       varchar(256) not null default "",
    server     varchar(128)
);

create table if not exists panels (
    id        integer      not null primary key,
    stream    integer      not null,
    text      text         not null,
    image     varchar(256) not null default ""
);`

func NewSQLDatabase(localhost string, driver string, server string) (Database, error) {
	db, err := sql.Open(driver, server)
	if err == nil {
		wrapped := &sqlDAO{DB: *db, localhost: localhost, streamTokens: make(map[string]string)}
		if err = wrapped.prepare(); err == nil {
			return wrapped, nil
		}
		wrapped.Close()
	}
	return nil, err
}

func (d *sqlDAO) prepare() error {
	if _, err := d.Exec(sqlSchema); err != nil {
		return err
	}
	t := reflect.TypeOf(&d.prepared).Elem()
	v := reflect.ValueOf(&d.prepared).Elem()
	for i := 0; i < t.NumField(); i++ {
		stmt, err := d.Prepare(string(t.Field(i).Tag))
		if err != nil {
			return err
		}
		v.Field(i).Set(reflect.ValueOf(stmt))
	}
	return nil
}

func (d *sqlDAO) userExists(login string, email string) bool {
	var i int
	return d.prepared.UserExists.QueryRow(login, email).Scan(&i) != sql.ErrNoRows
}

func (d *sqlDAO) NewUser(login string, email string, password []byte) (*UserData, error) {
	if err := ValidateUsername(login); err != nil {
		return nil, err
	}
	if err := ValidateEmail(email); err != nil {
		return nil, err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return nil, err
	}
	actoken := makeToken(tokenLength)
	sectoken := makeToken(tokenLength)
	r, err := d.prepared.NewUser.Exec(actoken, sectoken, login, login, email, hash)
	if err != nil {
		if d.userExists(login, email) {
			return nil, ErrUserNotUnique
		}
		return nil, err
	}

	uid, err := r.LastInsertId()
	if err == nil {
		_, err = d.prepared.NewStream.Exec(uid)
	}
	return &UserData{uid, login, email, login, hash, "", false, actoken, sectoken}, err
}

func (d *sqlDAO) ActivateUser(id int64, token string) error {
	r, err := d.prepared.ActivateUser.Exec(id, token)
	if err != nil {
		return err
	}
	changed, err := r.RowsAffected()
	if err == nil && changed != 1 {
		return ErrInvalidToken
	}
	return err
}

func (d *sqlDAO) GetUserID(login string, password []byte) (int64, error) {
	var u UserData
	err := d.prepared.GetUserID.QueryRow(login).Scan(&u.ID, &u.PwHash)
	if err == sql.ErrNoRows {
		err = ErrUserNotExist
	} else if err == nil {
		err = u.CheckPassword(password)
	}
	return u.ID, err
}

func (d *sqlDAO) GetUserFull(id int64) (*UserData, error) {
	var actoken sql.NullString
	u := UserData{ID: id}
	err := d.prepared.GetUserInfo.QueryRow(id).Scan(
		&u.Name, &u.Login, &u.Email, &u.PwHash, &u.About, &actoken, &u.StreamToken,
	)
	if err == sql.ErrNoRows {
		return nil, ErrUserNotExist
	}
	if u.Activated = !actoken.Valid; actoken.Valid {
		u.ActivationToken = actoken.String
	}
	return &u, err
}

func (d *sqlDAO) SetUserData(id int64, name string, login string, email string, about string, password []byte) (string, error) {
	token := ""
	query := "update users set "
	params := make([]interface{}, 0, 7)

	if name != "" {
		query += "name = ?, "
		params = append(params, name)
	}

	if login != "" {
		if err := ValidateUsername(login); err != nil {
			return "", err
		}
		query += "login = ?, "
		params = append(params, login)
	}

	if email != "" {
		if err := ValidateEmail(email); err != nil {
			return "", err
		}
		token = makeToken(tokenLength)
		query += "actoken = ?, email = ?, "
		params = append(params, token, email)
	}

	if len(password) != 0 {
		hash, err := hashPassword(password)
		if err != nil {
			return "", err
		}
		query += "password = ?, "
		params = append(params, hash)
	}

	query += "about = ? where id = ? and not exists(select 1 from streams where user = users.id and server is not null)"
	params = append(params, about, id)

	r, err := d.Exec(query, params...)
	if err != nil {
		if (name != "" || email != "") && d.userExists(name, email) {
			return "", ErrUserNotUnique
		}
		return "", err
	}
	rows, err := r.RowsAffected()
	if err == nil && rows != 1 {
		return "", ErrStreamActive
	}
	return token, err
}

func errOf(_ interface{}, err error) error {
	return err
}

func (d *sqlDAO) NewStreamToken(id int64) error {
	// TODO invalidate token cache on all nodes
	//      damn, it appears I ran into the most difficult problem...
	return errOf(d.prepared.SetStreamToken.Exec(makeToken(tokenLength), id))
}

func (d *sqlDAO) SetStreamName(id int64, name string) error {
	return errOf(d.prepared.SetStreamName.Exec(name, id))
}

func (d *sqlDAO) AddStreamPanel(id int64, text string) error {
	return errOf(d.prepared.AddStreamPanel.Exec(text, id))
}

func (d *sqlDAO) SetStreamPanel(id int64, n int64, text string) error {
	return errOf(d.prepared.SetStreamPanel.Exec(text, id, n))
}

func (d *sqlDAO) DelStreamPanel(id int64, n int64) error {
	return errOf(d.prepared.DelStreamPanel.Exec(id, n))
}

func (d *sqlDAO) StartStream(id string, token string) error {
	d.streamTokenLock.RLock()
	if expect, ok := d.streamTokens[id]; ok {
		d.streamTokenLock.RUnlock()
		if expect != token {
			return ErrInvalidToken
		}
		return nil
	}
	d.streamTokenLock.RUnlock()

	_, err := d.prepared.SetStreamServer.Exec(d.localhost, id, token)
	if err != nil {
		return err
	}

	var expect string
	var server sql.NullString
	var activated = true

	err = d.prepared.GetStreamAuth.QueryRow(id).Scan(&server, &expect, &activated)
	if err == sql.ErrNoRows {
		return ErrStreamNotExist
	}
	if err != nil {
		return err
	}
	if expect != token || !activated {
		return ErrInvalidToken
	}
	if !server.Valid || server.String != d.localhost {
		return ErrStreamNotHere
	}
	d.streamTokenLock.Lock()
	d.streamTokens[id] = expect
	d.streamTokenLock.Unlock()
	return nil
}

func (d *sqlDAO) StopStream(id string) error {
	d.streamTokenLock.Lock()
	delete(d.streamTokens, id)
	d.streamTokenLock.Unlock()
	_, err := d.prepared.DelStreamServer.Exec(id)
	return err
}

func (d *sqlDAO) GetStreamServer(id string) (string, error) {
	d.streamTokenLock.RLock()
	if _, ok := d.streamTokens[id]; ok {
		d.streamTokenLock.RUnlock()
		return d.localhost, nil
	}
	d.streamTokenLock.RUnlock()

	var server sql.NullString
	err := d.prepared.GetStreamServer.QueryRow(id).Scan(&server)
	if err == sql.ErrNoRows {
		return "", ErrStreamNotExist
	}
	if err != nil {
		return "", err
	}
	if !server.Valid {
		return "", ErrStreamOffline
	}
	if server.String != d.localhost {
		return server.String, ErrStreamNotHere
	}
	if _, err = d.prepared.DelStreamServer.Exec(id); err != nil {
		return "", err
	}
	return "", ErrStreamOffline
}

func (d *sqlDAO) GetStreamMetadata(id string) (*StreamMetadata, error) {
	var intId int
	var server sql.NullString
	meta := StreamMetadata{}
	err := d.prepared.GetStreamInfo.QueryRow(id).Scan(
		&meta.UserName, &meta.UserAbout, &meta.Email, &meta.Name, &server, &meta.HasVideo, &meta.HasAudio, &meta.Width, &meta.Height, &intId,
	)
	if err == sql.ErrNoRows {
		return nil, ErrStreamNotExist
	}
	if err != nil {
		return nil, err
	}
	rows, err := d.prepared.GetStreamPanels.Query(intId)
	if err == nil {
		var panel StreamMetadataPanel
		for rows.Next() {
			if err = rows.Scan(&panel.Text, &panel.Image); err != nil {
				break
			}
			meta.Panels = append(meta.Panels, panel)
		}
		if err = rows.Err(); err == nil && !server.Valid {
			err = ErrStreamOffline
		}
		rows.Close()
		meta.Server = server.String
	}
	return &meta, err
}

func (d *sqlDAO) SetStreamTrackInfo(id string, info *StreamTrackInfo) error {
	return errOf(d.prepared.SetStreamTracks.Exec(info.HasVideo, info.HasAudio, info.Width, info.Height, id))
}