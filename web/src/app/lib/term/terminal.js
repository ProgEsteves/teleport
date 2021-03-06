/*
Copyright 2015 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
import Term from 'xterm/dist/xterm';
import Tty from './tty';
import TtyEvents from './ttyEvents';
import {debounce, isNumber} from 'lodash';
import api from 'app/services/api';
import Logger from 'app/lib/logger';
import $ from 'jQuery';

Term.colors[256] = '#252323';

const logger = Logger.create('lib/term/terminal');
const DISCONNECT_TXT = 'disconnected';
const GRV_CLASS = 'grv-terminal';
const WINDOW_RESIZE_DEBOUNCE_DELAY = 200;

/**
 * TtyTerminal is a wrapper on top of xtermjs that handles connections
 * and resize events
 */
class TtyTerminal {

  constructor(options){
    let {
      tty,
      scrollBack = 1000 } = options;

    this.ttyParams = tty;
    this.tty = new Tty();
    this.ttyEvents = new TtyEvents();

    this.scrollBack = scrollBack
    this.rows = undefined;
    this.cols = undefined;
    this.term = null;
    this._el = options.el;

    this.debouncedResize = debounce(
      this._requestResize.bind(this),
      WINDOW_RESIZE_DEBOUNCE_DELAY
    );
  }

  open() {
    $(this._el).addClass(GRV_CLASS);

    // render xtermjs with default values
    this.term = new Term({    
      cols: 15,
      rows: 5,
      scrollback: this.scrollBack,                  
      cursorBlink: false
    });
    
    this.term.open(this._el);

    // resize xterm to available space
    this.resize(this.cols, this.rows);

    // subscribe to xtermjs output
    this.term.on('data', data => this.tty.send(data));
    
    // subscribe to tty
    this.tty.on('reset', this.reset.bind(this));    
    this.tty.on('close', this._processClose.bind(this));
    this.tty.on('data', this._processData.bind(this));    
    
    // subscribe tty resize event (used by session player)
    this.tty.on('resize', ({h, w}) => this.resize(w, h));    
    // subscribe to window resize events
    window.addEventListener('resize', this.debouncedResize);
    // subscribe to session resize events (triggered by other participants)
    this.ttyEvents.on('resize', ({h, w}) => this.resize(w, h));    

    this.connect();    
  }
  
  connect(){
    this.tty.connect(this._getTtyConnStr());
    this.ttyEvents.connect(this._getTtyEventsConnStr());
  }

  destroy() {
    window.removeEventListener('resize', this.debouncedResize);
    this._disconnect();
    if(this.term !== null){
      this.term.destroy();
      this.term.removeAllListeners();
    }

    $(this._el).empty().removeClass(GRV_CLASS);    
  }

  reset() {        
    this.term.reset()
  }

  resize(cols, rows) {        
    try {      
      // if not defined, use the size of the container
      if(!isNumber(cols) || !isNumber(rows)){
        let dim = this._getDimensions();
        cols = dim.cols;
        rows = dim.rows;
      }

      if(cols === this.cols && rows === this.rows){
        return;
      }

      this.cols = cols;
      this.rows = rows;    

      this.term.resize(cols, rows);  
    } catch (err) {            
      logger.error('resize', { w: cols, h: rows }, err);     
      this.term.reset();  
    }       
  }

  _processData(data){
    try {                  
      this.term.write(data);                    
    } catch (err) {            
      logger.error('xterm.write', data, err);
      // reset xtermjs so it can recover
      this.term.reset();  
    }
  }
    
  _processClose(e) {
    let { reason } = e;
    let displayText = DISCONNECT_TXT;
            
    if (reason) {
      displayText = `${displayText}: ${reason}`;
    }
                    
    displayText = `\x1b[31m${displayText}\x1b[m\r\n`;
    this.term.write(displayText)
  }

  _disconnect() {        
    this.tty.disconnect();
    this.tty.removeAllListeners();    
    this.ttyEvents.disconnect();
    this.ttyEvents.removeAllListeners();    
  }

  _requestResize(){
    let {cols, rows} = this._getDimensions();
    let w = cols;
    let h = rows;

    // some min values
    w = w < 5 ? 5 : w;
    h = h < 5 ? 5 : h;

    let { sid, url } = this.ttyParams;
    let reqData = { terminal_params: { w, h } };
    
    logger.info('requesting new screen size', `w:${w} and h:${h}`);    
    this.resize(w, h);
    api.put(`${url}/sessions/${sid}`, reqData)      
      .fail(err => logger.error('request new screen size', err));
  }

  _getDimensions(){
    let $container = $(this._el);
    let fakeRow = $('<div><span>&nbsp;</span></div>');

    $container.find('.terminal').append(fakeRow);
    // get div height
    let fakeColHeight = fakeRow[0].getBoundingClientRect().height;
    // get span width
    let fakeColWidth = fakeRow.children().first()[0].getBoundingClientRect().width;

    let width = $container[0].clientWidth;
    let height = $container[0].clientHeight;

    let cols = Math.floor(width / (fakeColWidth));
    let rows = Math.floor(height / (fakeColHeight));
    fakeRow.remove();

    return {cols, rows};
  }

  _getTtyEventsConnStr(){
    let {sid, url, token } = this.ttyParams;
    let urlPrefix = getWsHostName();
    return `${urlPrefix}${url}/sessions/${sid}/events/stream?access_token=${token}`;
  }

  _getTtyConnStr(){
    let {serverId, login, sid, url, token } = this.ttyParams;
    let params = {
      server_id: serverId,
      login,
      sid,
      term: {
        h: this.rows,
        w: this.cols
      }
    }

    let json = JSON.stringify(params);
    let jsonEncoded = window.encodeURI(json);
    let urlPrefix = getWsHostName();

    return `${urlPrefix}${url}/connect?access_token=${token}&params=${jsonEncoded}`;
  }  
}

function getWsHostName(){
  var prefix = location.protocol == "https:"?"wss://":"ws://";
  var hostport = location.hostname+(location.port ? ':'+location.port: '');
  return `${prefix}${hostport}`;
}

export default TtyTerminal;