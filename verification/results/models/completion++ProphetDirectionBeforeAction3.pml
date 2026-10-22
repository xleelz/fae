// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go#L3
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_ProphetDirectionBeforeAction30 = [1] of {int}->
	run ProphetDirectionBeforeAction3(child_ProphetDirectionBeforeAction30)->
	child_ProphetDirectionBeforeAction30?0->
stop_process:skip
}

proctype ProphetDirectionBeforeAction3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousProphetDirectionBeforeAction952 = [1] of {int}->
	chan child_AnonymousProphetDirectionBeforeAction861 = [1] of {int}->
	chan child_AnonymousProphetDirectionBeforeAction740 = [1] of {int}->
	Chandef reply_ch->
	Chandef action_ch->
	Chandef direction_ch->
	run sync_monitor(direction_ch)->
	run sync_monitor(action_ch)->
	run sync_monitor(reply_ch)->
	run AnonymousProphetDirectionBeforeAction74(direction_ch,action_ch,reply_ch,child_AnonymousProphetDirectionBeforeAction740)->
	run receiver(child_AnonymousProphetDirectionBeforeAction740)->
	run AnonymousProphetDirectionBeforeAction86(reply_ch,direction_ch,action_ch,child_AnonymousProphetDirectionBeforeAction861)->
	run receiver(child_AnonymousProphetDirectionBeforeAction861)->
	run AnonymousProphetDirectionBeforeAction95(action_ch,reply_ch,direction_ch,child_AnonymousProphetDirectionBeforeAction952)->
	run receiver(child_AnonymousProphetDirectionBeforeAction952)->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:10:2 */
	:: reply_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:10:2 */->
	:: reply_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:10:2 */ -> 
		reply_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousProphetDirectionBeforeAction74(Chandef direction_ch;Chandef action_ch;Chandef reply_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:7:14 */
	:: direction_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:7:14 */
	:: direction_ch.sync!false -> 
		direction_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:7:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousProphetDirectionBeforeAction86(Chandef reply_ch;Chandef direction_ch;Chandef action_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:8:14 */
	:: direction_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:8:14 */->
	:: direction_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:8:14 */ -> 
		direction_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:8:27 */
	:: action_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:8:27 */
	:: action_ch.sync!false -> 
		action_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:8:27 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousProphetDirectionBeforeAction95(Chandef action_ch;Chandef reply_ch;Chandef direction_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:9:14 */
	:: action_ch.deq?state,num_msgs /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:9:14 */->
	:: action_ch.sync?state /* 	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:9:14 */ -> 
		action_ch.rcving!false
	fi->
	

	if /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:9:24 */
	:: reply_ch.enq?ok -> 
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:9:24 */
	:: reply_ch.sync!false -> 
		reply_ch.sending?ok->
		assert(ok) /* Send	/Users/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.IirR6M/source/prophet.go:9:24 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

